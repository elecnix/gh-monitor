package monitor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/elecnix/gh-monitor/internal/ghcli"
)

// IsRateLimitError reports whether err is GitHub refusing a call because the
// budget for its resource is spent: a GraphQL "rate limit" error, a REST
// error whose message says so, or HTTP 429.
func IsRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	var gqlErr *ghcli.GraphQLError
	if errors.As(err, &gqlErr) {
		return strings.Contains(strings.ToLower(gqlErr.Error()), "rate limit")
	}
	var apiErr *ghcli.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.ContainsLower("rate limit")
	}
	return false
}

// restPull is the part of GET /repos/{owner}/{repo}/pulls/{n} a REST read uses.
type restPull struct {
	State          string `json:"state"` // open, closed
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"` // null while GitHub computes it
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		Sha string `json:"sha"`
	} `json:"head"`
}

type restCheckRuns struct {
	TotalCount int            `json:"total_count"`
	CheckRuns  []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	StartedAt   string  `json:"started_at"`
	CompletedAt string  `json:"completed_at"`
	DetailsURL  string  `json:"details_url"`
	HTMLURL     string  `json:"html_url"`
	App         struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"app"`
	CheckSuite struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
}

type restCombinedStatus struct {
	Statuses []struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	} `json:"statuses"`
}

type restCommit struct {
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

// maxCheckRunPages bounds the check-runs pages one REST read fetches. At 100
// runs a page this covers more runs than the GraphQL query's 50 suites.
const maxCheckRunPages = 5

// FetchPRViaREST reads a PR's state, mergeability, head commit and check
// outcomes over REST, for when the GraphQL budget is spent (issue #123). The
// REST and GraphQL budgets are separate, so REST usually still has room.
//
// The result has the shape of a TierStatus GraphQL fetch: comments, review
// threads, reviews and annotations are left empty, and the caller reports them
// as shed. prev is the last payload of this PR, if any. When its head commit
// matches, the read reuses that commit's message and authors and skips the
// commit read.
func (s *Service) FetchPRViaREST(owner, repo string, number int, prev *PullRequest) (*PullRequest, error) {
	var pull restPull
	if err := s.API.REST("GET", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number), nil, nil, &pull); err != nil {
		return nil, fmt.Errorf("read pull request over REST: %w", err)
	}
	pr := &PullRequest{
		State:      strings.ToUpper(pull.State),
		Merged:     pull.Merged,
		Mergeable:  "UNKNOWN",
		MergeState: strings.ToUpper(pull.MergeableState),
	}
	if pull.Merged {
		pr.State = "MERGED"
	}
	if pull.Mergeable != nil {
		if *pull.Mergeable {
			pr.Mergeable = "MERGEABLE"
		} else {
			pr.Mergeable = "CONFLICTING"
		}
	}
	if pull.Head.Sha == "" {
		return pr, nil
	}

	details := CommitDetails{Oid: pull.Head.Sha}
	if known, ok := headCommit(prev); ok && known.Oid == pull.Head.Sha {
		details.MessageHeadline = known.MessageHeadline
		details.Message = known.Message
		details.Authors = known.Authors
	} else {
		var c restCommit
		if err := s.API.REST("GET", fmt.Sprintf("repos/%s/%s/commits/%s", owner, repo, pull.Head.Sha), nil, nil, &c); err != nil {
			return nil, fmt.Errorf("read head commit over REST: %w", err)
		}
		details.Message = c.Commit.Message
		details.MessageHeadline, _, _ = strings.Cut(c.Commit.Message, "\n")
		actor := GitActor{Name: c.Commit.Author.Name}
		if c.Author != nil && c.Author.Login != "" {
			actor.User = &struct {
				Login string `json:"login"`
			}{Login: c.Author.Login}
		}
		details.Authors.Nodes = []GitActor{actor}
	}

	suites, err := s.restCheckSuites(owner, repo, pull.Head.Sha)
	if err != nil {
		return nil, err
	}
	details.CheckSuites = suites

	var combined restCombinedStatus
	if err := s.API.REST("GET", fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, repo, pull.Head.Sha), nil, nil, &combined); err != nil {
		return nil, fmt.Errorf("read commit status over REST: %w", err)
	}
	if len(combined.Statuses) > 0 {
		details.Status = &CommitStatus{}
		for _, st := range combined.Statuses {
			details.Status.Contexts = append(details.Status.Contexts, StatusContext{
				State:       strings.ToUpper(st.State),
				Context:     st.Context,
				Description: st.Description,
				TargetURL:   st.TargetURL,
			})
		}
	}

	pr.Commits.Nodes = []Commit{{Commit: details}}
	return pr, nil
}

// restCheckSuites reads the head commit's check runs and groups them into
// suites by check suite ID, in the order GitHub lists them. Enum values are
// upper-cased to match GraphQL. A suite is COMPLETED when every run in it is,
// and otherwise takes the status of its first unfinished run, so the pending
// classifier reports it as it does for a GraphQL payload.
func (s *Service) restCheckSuites(owner, repo, sha string) (SuiteNodes, error) {
	var runs []restCheckRun
	for page := 1; page <= maxCheckRunPages; page++ {
		var resp restCheckRuns
		path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", owner, repo, sha, page)
		if err := s.API.REST("GET", path, nil, nil, &resp); err != nil {
			return SuiteNodes{}, fmt.Errorf("read check runs over REST: %w", err)
		}
		runs = append(runs, resp.CheckRuns...)
		if len(resp.CheckRuns) == 0 || len(runs) >= resp.TotalCount {
			break
		}
	}

	var out SuiteNodes
	index := map[int64]int{}
	for _, r := range runs {
		i, ok := index[r.CheckSuite.ID]
		if !ok {
			i = len(out.Nodes)
			index[r.CheckSuite.ID] = i
			out.Nodes = append(out.Nodes, CheckSuite{
				Status: "COMPLETED",
				App:    AppInfo{Name: r.App.Name, Slug: r.App.Slug},
			})
		}
		suite := &out.Nodes[i]
		run := CheckRun{
			Name:        r.Name,
			Status:      strings.ToUpper(r.Status),
			StartedAt:   r.StartedAt,
			CompletedAt: r.CompletedAt,
			DetailsURL:  r.DetailsURL,
			Permalink:   r.HTMLURL,
		}
		if r.Conclusion != nil {
			run.Conclusion = strings.ToUpper(*r.Conclusion)
		}
		if run.Status != "COMPLETED" && suite.Status == "COMPLETED" {
			suite.Status = run.Status
		}
		suite.CheckRuns.Nodes = append(suite.CheckRuns.Nodes, run)
	}
	for i := range out.Nodes {
		out.Nodes[i].CheckRuns.TotalCount = len(out.Nodes[i].CheckRuns.Nodes)
	}
	out.TotalCount = len(out.Nodes)
	return out, nil
}

// headCommit returns the head commit of a PR payload, if it has one.
func headCommit(pr *PullRequest) (CommitDetails, bool) {
	if pr == nil || len(pr.Commits.Nodes) == 0 {
		return CommitDetails{}, false
	}
	return pr.Commits.Nodes[0].Commit, true
}
