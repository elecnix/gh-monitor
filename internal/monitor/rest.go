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

type restCheckSuites struct {
	TotalCount  int              `json:"total_count"`
	CheckSuites []restCheckSuite `json:"check_suites"`
}

type restCheckSuite struct {
	ID         int64   `json:"id"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	App        struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"app"`
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

// maxCheckPages bounds the pages of check suites, and of check runs, one REST
// read fetches. At 100 a page this covers more than the GraphQL query's 50
// suites of 50 runs each.
const maxCheckPages = 5

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

// restCheckSuites reads the head commit's check suites and check runs, and
// attaches each run to its suite. Enum values are upper-cased to match
// GraphQL.
//
// The suites come from the check-suites endpoint, because the check-runs
// endpoint cannot list a suite that has no runs. GraphQL returns such a suite,
// and the classifiers count a runless third-party suite as a check: a queued
// one is pending and a failed one is failing. Without it a REST read reported
// CI green while those suites were still queued (review of #124).
//
// When the page cap stops either read before its total_count, the result's
// TotalCount is set above the number of suites read, so the snapshot reports
// TruncatedSuites and ciAllGreen refuses to call the payload green.
func (s *Service) restCheckSuites(owner, repo, sha string) (SuiteNodes, error) {
	var out SuiteNodes
	index := map[int64]int{}
	addSuite := func(id int64, suite CheckSuite) int {
		if i, ok := index[id]; ok {
			return i
		}
		index[id] = len(out.Nodes)
		out.Nodes = append(out.Nodes, suite)
		return len(out.Nodes) - 1
	}

	suitesTotal, suitesRead := 0, 0
	for page := 1; page <= maxCheckPages; page++ {
		var resp restCheckSuites
		path := fmt.Sprintf("repos/%s/%s/commits/%s/check-suites?per_page=100&page=%d", owner, repo, sha, page)
		if err := s.API.REST("GET", path, nil, nil, &resp); err != nil {
			return SuiteNodes{}, fmt.Errorf("read check suites over REST: %w", err)
		}
		suitesTotal = resp.TotalCount
		for _, cs := range resp.CheckSuites {
			suite := CheckSuite{
				Status: strings.ToUpper(cs.Status),
				App:    AppInfo{Name: cs.App.Name, Slug: cs.App.Slug},
			}
			if cs.Conclusion != nil {
				suite.Conclusion = strings.ToUpper(*cs.Conclusion)
			}
			addSuite(cs.ID, suite)
		}
		suitesRead += len(resp.CheckSuites)
		if len(resp.CheckSuites) == 0 || suitesRead >= resp.TotalCount {
			break
		}
	}

	runsTotal, runsRead := 0, 0
	for page := 1; page <= maxCheckPages; page++ {
		var resp restCheckRuns
		path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", owner, repo, sha, page)
		if err := s.API.REST("GET", path, nil, nil, &resp); err != nil {
			return SuiteNodes{}, fmt.Errorf("read check runs over REST: %w", err)
		}
		runsTotal = resp.TotalCount
		for _, r := range resp.CheckRuns {
			// A run whose suite was past the suites page cap still counts:
			// its suite is built from the run's app, and marked unfinished
			// when the run is.
			i := addSuite(r.CheckSuite.ID, CheckSuite{
				Status: "COMPLETED",
				App:    AppInfo{Name: r.App.Name, Slug: r.App.Slug},
			})
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
			out.Nodes[i].CheckRuns.Nodes = append(out.Nodes[i].CheckRuns.Nodes, run)
		}
		runsRead += len(resp.CheckRuns)
		if len(resp.CheckRuns) == 0 || runsRead >= resp.TotalCount {
			break
		}
	}

	for i := range out.Nodes {
		out.Nodes[i].CheckRuns.TotalCount = len(out.Nodes[i].CheckRuns.Nodes)
	}
	out.TotalCount = len(out.Nodes)
	if suitesTotal > out.TotalCount {
		out.TotalCount = suitesTotal
	}
	if runsRead < runsTotal && out.TotalCount <= len(out.Nodes) {
		// The unread runs may belong to suites this read never saw.
		out.TotalCount = len(out.Nodes) + 1
	}
	return out, nil
}

// headCommit returns the head commit of a PR payload, if it has one.
func headCommit(pr *PullRequest) (CommitDetails, bool) {
	if pr == nil || len(pr.Commits.Nodes) == 0 {
		return CommitDetails{}, false
	}
	return pr.Commits.Nodes[0].Commit, true
}
