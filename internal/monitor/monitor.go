// Package monitor provides a change-detection / snapshot engine for a pull
// request. It fetches a rich GraphQL snapshot, distills it into a stable
// PRStatus, and diffs two snapshots into a set of Events describing what
// genuinely changed. A future `monitor` command consumes this engine.
//
// The logic is ported from the pi-ghpr-monitor TypeScript extension
// (analyzer.ts): the 👍-acknowledgement filtering, co-author trailer parsing,
// and "is this thread new" dedup all mirror that implementation. It duplicates
// the failing/pending check classifiers
// (they are unexported there) so this package stays self-contained.
package monitor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// Service fetches PR monitoring data through the GitHub API.
type Service struct {
	API ghcli.API

	// FailedRunLogsFn returns the failed-run log output for a workflow run —
	// typically the stdout of `gh run view <run-id> --log-failed`, which
	// combines each failing job's name with its error log lines. It is optional:
	// when nil, run-completed notifications for failed runs carry no log
	// snippet. Production code wires this to ghcli.Client.FailedRunLogs via cmd.
	FailedRunLogsFn func(owner, repo string, runID int) (string, error)
}

// FailedRunLogs returns the failed-run log output for a workflow run, or ""
// with a nil error when no fetcher is configured (so callers can invoke it
// unconditionally without guarding the optional field).
func (s *Service) FailedRunLogs(owner, repo string, runID int) (string, error) {
	if s.FailedRunLogsFn == nil {
		return "", nil
	}
	return s.FailedRunLogsFn(owner, repo, runID)
}

// MONITOR_QUERY fetches the rich snapshot a monitor needs for a single PR:
// state, comments (+👍), review threads (+👍), mergeability, the latest review
// decision, and the head commit with its authors, message, checks, and
// old-style statuses. The commit author/message fields are intentionally
// richer than pi's snapshot: pi documented author/co-author support but never
// wired the query, so we fetch it here.
const MONITOR_QUERY = `query MonitorPR($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      state
      merged
      mergeable
      mergeStateStatus
      comments(last: 25) {
        nodes {
          id
          body
          author { login }
          createdAt
          reactionGroups { content users { totalCount } }
        }
      }
      reviewThreads(last: 25) {
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(last: 25) {
            nodes {
              id
              body
              author { login }
              createdAt
              diffHunk
              reactionGroups { content users { totalCount } }
            }
          }
        }
      }
      reviews(last: 100) {
        nodes { state author { login } submittedAt }
      }
      commits(last: 1) {
        nodes {
          commit {
            oid
            messageHeadline
            message
            authors(first: 10) { nodes { name user { login } } }
            checkSuites(last: 10) {
              nodes {
                conclusion
                status
                app { name slug }
                checkRuns(last: 10) {
                  nodes { name conclusion status
                    annotations(first: 50) {
                      nodes { path location { start { line } } annotationLevel title message }
                    }
                  }
                }
              }
            }
            status { contexts { state context description targetUrl } }
          }
        }
      }
    }
  }
}`

// QueryResponse mirrors the GraphQL envelope's data shape.
type QueryResponse struct {
	Repository struct {
		PullRequest *PullRequest `json:"pullRequest"`
	} `json:"repository"`
}

// PullRequest is the raw GraphQL PR payload.
type PullRequest struct {
	State         string       `json:"state"`
	Merged        bool         `json:"merged"`
	Mergeable     string       `json:"mergeable"`
	MergeState    string       `json:"mergeStateStatus"`
	Comments      CommentNodes `json:"comments"`
	ReviewThreads ThreadNodes  `json:"reviewThreads"`
	Reviews       ReviewNodes  `json:"reviews"`
	Commits       CommitNodes  `json:"commits"`
}

type CommentNodes struct {
	Nodes []Comment `json:"nodes"`
}

// Comment covers both IssueComment (general) and PullRequestReviewComment
// (in-thread) shapes; path/line are only populated for review comments.
type Comment struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	CreatedAt      string          `json:"createdAt"`
	ReactionGroups []ReactionGroup `json:"reactionGroups"`
	Path           string          `json:"path"`
	Line           *int            `json:"line"`
	DiffHunk       string          `json:"diffHunk"`
}

// ReactionGroup is one content bucket of a comment's reactions.
type ReactionGroup struct {
	Content string `json:"content"`
	Users   struct {
		TotalCount int `json:"totalCount"`
	} `json:"users"`
}

type ThreadNodes struct {
	Nodes []ReviewThread `json:"nodes"`
}

type ReviewThread struct {
	ID         string       `json:"id"`
	IsResolved bool         `json:"isResolved"`
	IsOutdated bool         `json:"isOutdated"`
	Path       string       `json:"path"`
	Line       *int         `json:"line"`
	Comments   CommentNodes `json:"comments"`
}

type ReviewNodes struct {
	Nodes []Review `json:"nodes"`
}

type Review struct {
	State  string `json:"state"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	SubmittedAt string `json:"submittedAt"`
}

type CommitNodes struct {
	Nodes []Commit `json:"nodes"`
}

type Commit struct {
	Commit CommitDetails `json:"commit"`
}

type CommitDetails struct {
	Oid             string        `json:"oid"`
	MessageHeadline string        `json:"messageHeadline"`
	Message         string        `json:"message"`
	Authors         GitActorNodes `json:"authors"`
	CheckSuites     SuiteNodes    `json:"checkSuites"`
	Status          *CommitStatus `json:"status"`
}

type GitActorNodes struct {
	Nodes []GitActor `json:"nodes"`
}

type GitActor struct {
	Name string `json:"name"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
}

type SuiteNodes struct {
	Nodes []CheckSuite `json:"nodes"`
}

type CheckSuite struct {
	Conclusion string   `json:"conclusion"`
	Status     string   `json:"status"`
	App        AppInfo  `json:"app"`
	CheckRuns  RunNodes `json:"checkRuns"`
}

type AppInfo struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type RunNodes struct {
	Nodes []CheckRun `json:"nodes"`
}

type CheckRun struct {
	Name        string          `json:"name"`
	Conclusion  string          `json:"conclusion"`
	Status      string          `json:"status"`
	Annotations AnnotationNodes `json:"annotations"`
}

type AnnotationNodes struct {
	Nodes []Annotation `json:"nodes"`
}

// Annotation is a raw GraphQL check-run annotation.
type Annotation struct {
	Path     string             `json:"path"`
	Location AnnotationLocation `json:"location"`
	Level    string             `json:"annotationLevel"`
	Title    string             `json:"title"`
	Message  string             `json:"message"`
}

// AnnotationLocation holds the start line of an annotation.
type AnnotationLocation struct {
	Start struct {
		Line int `json:"line"`
	} `json:"start"`
}

type CommitStatus struct {
	Contexts []StatusContext `json:"contexts"`
}

type StatusContext struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"targetUrl"`
}

// Fetch retrieves the monitoring snapshot for a PR.
func (s *Service) Fetch(identity *resolver.Identity, number int) (*QueryResponse, error) {
	var result QueryResponse
	err := s.API.GraphQL(MONITOR_QUERY, map[string]interface{}{
		"owner":  identity.Owner,
		"repo":   identity.Repo,
		"number": number,
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.Repository.PullRequest == nil {
		return nil, fmt.Errorf("pull request not found or not accessible")
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Ref / commit monitoring
// ---------------------------------------------------------------------------

// MONITOR_REF_QUERY fetches the commit at the tip of a ref along with its
// check suites and status contexts.
const MONITOR_REF_QUERY = `query MonitorRef($owner: String!, $repo: String!, $ref: String!) {
  repository(owner: $owner, name: $repo) {
    ref(qualifiedName: $ref) {
      target {
        oid
        ... on Commit {
          messageHeadline
          authors(first: 10) { nodes { name user { login } } }
          checkSuites(last: 10) {
            nodes {
              conclusion
              status
              app { name slug }
              checkRuns(last: 10) {
                nodes { name conclusion status
                  annotations(first: 50) {
                    nodes { path location { start { line } } annotationLevel title message }
                  }
                }
              }
            }
          }
          status { contexts { state context description targetUrl } }
        }
      }
    }
  }
}`

// MONITOR_COMMIT_QUERY fetches a specific commit by OID along with its check
// suites and status contexts.
const MONITOR_COMMIT_QUERY = `query MonitorCommit($owner: String!, $repo: String!, $oid: GitObjectID!) {
  repository(owner: $owner, name: $repo) {
    object(oid: $oid) {
      ... on Commit {
        oid
        messageHeadline
        authors(first: 10) { nodes { name user { login } } }
        checkSuites(last: 10) {
          nodes {
            conclusion
            status
            app { name slug }
            checkRuns(last: 10) {
              nodes { name conclusion status
                annotations(first: 50) {
                  nodes { path location { start { line } } annotationLevel title message }
                }
              }
            }
          }
        }
        status { contexts { state context description targetUrl } }
      }
    }
  }
}`

// RefQueryResponse mirrors the GraphQL envelope for a ref query.
type RefQueryResponse struct {
	Repository struct {
		Ref *RefTarget `json:"ref"`
	} `json:"repository"`
}

// RefTarget holds the tip commit of a ref.
type RefTarget struct {
	Target struct {
		Oid             string        `json:"oid"`
		MessageHeadline string        `json:"messageHeadline"`
		Authors         GitActorNodes `json:"authors"`
		CheckSuites     SuiteNodes    `json:"checkSuites"`
		Status          *CommitStatus `json:"status"`
	} `json:"target"`
}

// CommitQueryResponse mirrors the GraphQL envelope for a commit query.
type CommitQueryResponse struct {
	Repository struct {
		Object *CommitObject `json:"object"`
	} `json:"repository"`
}

// CommitObject is the commit returned by repository.object.
type CommitObject struct {
	Oid             string        `json:"oid"`
	MessageHeadline string        `json:"messageHeadline"`
	Authors         GitActorNodes `json:"authors"`
	CheckSuites     SuiteNodes    `json:"checkSuites"`
	Status          *CommitStatus `json:"status"`
}

// FetchRef retrieves the monitoring snapshot for a branch ref.
func (s *Service) FetchRef(owner, repo, ref string) (*RefQueryResponse, error) {
	var result RefQueryResponse
	err := s.API.GraphQL(MONITOR_REF_QUERY, map[string]interface{}{
		"owner": owner,
		"repo":  repo,
		"ref":   ref,
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.Repository.Ref == nil {
		return nil, fmt.Errorf("ref not found or not accessible")
	}
	return &result, nil
}

// FetchCommit retrieves the monitoring snapshot for a commit SHA.
func (s *Service) FetchCommit(owner, repo, sha string) (*CommitQueryResponse, error) {
	var result CommitQueryResponse
	err := s.API.GraphQL(MONITOR_COMMIT_QUERY, map[string]interface{}{
		"owner": owner,
		"repo":  repo,
		"oid":   sha,
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.Repository.Object == nil {
		return nil, fmt.Errorf("commit not found or not accessible")
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Workflow-run monitoring (GitHub Actions)
// ---------------------------------------------------------------------------

// WorkflowRun is the relevant subset of a GitHub Actions run returned by the
// REST endpoint GET /repos/{owner}/{repo}/actions/runs/{run_id}.
type WorkflowRun struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	HTMLURL      string `json:"html_url"`
	RunNumber    int    `json:"run_number"`
}

// RunStatus is the stable snapshot for a workflow-run target.
type RunStatus struct {
	RunID        int    `json:"run_id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Event        string `json:"event"`
	Status       string `json:"status"` // queued | in_progress | completed
	Conclusion   string `json:"conclusion"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	ShortSHA     string `json:"short_sha"`
	HTMLURL      string `json:"html_url"`
	RunNumber    int    `json:"run_number"`
}

// IsTerminal reports whether the run has reached a final state.
func (r *RunStatus) IsTerminal() bool { return r.Status == "completed" }

// FetchRun retrieves the monitoring snapshot for a single workflow run via the
// REST API. Unlike the GraphQL PR/ref/issue fetchers, workflow runs are only
// available over REST.
func (s *Service) FetchRun(owner, repo string, runID int) (*WorkflowRun, error) {
	var result WorkflowRun
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d", owner, repo, runID)
	if err := s.API.REST("GET", path, nil, nil, &result); err != nil {
		return nil, err
	}
	if result.ID == 0 {
		return nil, fmt.Errorf("workflow run %d not found or not accessible", runID)
	}
	return &result, nil
}

// SnapshotRun distills a WorkflowRun into a RunStatus.
func SnapshotRun(run *WorkflowRun) *RunStatus {
	short := run.HeadSHA
	if len(short) > 7 {
		short = short[:7]
	}
	return &RunStatus{
		RunID:        run.ID,
		Name:         run.Name,
		DisplayTitle: run.DisplayTitle,
		Event:        run.Event,
		Status:       run.Status,
		Conclusion:   run.Conclusion,
		HeadBranch:   run.HeadBranch,
		HeadSHA:      run.HeadSHA,
		ShortSHA:     short,
		HTMLURL:      run.HTMLURL,
		RunNumber:    run.RunNumber,
	}
}

// ---------------------------------------------------------------------------
// Issue monitoring
// ---------------------------------------------------------------------------

// MONITOR_ISSUE_QUERY fetches an issue with its state, title, and latest
// comments.
const MONITOR_ISSUE_QUERY = `query MonitorIssue($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    issue(number: $number) {
      state
      title
      comments(last: 25) {
        nodes {
          id
          body
          author { login }
          createdAt
          reactionGroups { content users { totalCount } }
        }
      }
    }
  }
}`

// IssueQueryResponse mirrors the GraphQL envelope for an issue query.
type IssueQueryResponse struct {
	Repository struct {
		Issue *IssueNode `json:"issue"`
	} `json:"repository"`
}

// IssueNode is the raw GraphQL issue payload.
type IssueNode struct {
	State    string            `json:"state"`
	Title    string            `json:"title"`
	Comments IssueCommentNodes `json:"comments"`
}

// IssueCommentNodes holds the list of issue comments.
type IssueCommentNodes struct {
	Nodes []IssueComment `json:"nodes"`
}

// IssueComment is a single issue comment.
type IssueComment struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	CreatedAt      string          `json:"createdAt"`
	ReactionGroups []ReactionGroup `json:"reactionGroups"`
}

// FetchIssue retrieves the monitoring snapshot for an issue.
func (s *Service) FetchIssue(owner, repo string, number int) (*IssueQueryResponse, error) {
	var result IssueQueryResponse
	err := s.API.GraphQL(MONITOR_ISSUE_QUERY, map[string]interface{}{
		"owner":  owner,
		"repo":   repo,
		"number": number,
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.Repository.Issue == nil {
		return nil, fmt.Errorf("issue not found or not accessible")
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Snapshot types
// ---------------------------------------------------------------------------

// ThreadSummary is a distilled unresolved review thread.
type ThreadSummary struct {
	ID         string   `json:"id"`
	Path       string   `json:"path,omitempty"`
	Line       *int     `json:"line,omitempty"`
	CommentIDs []string `json:"comment_ids"`
	// Author and Body come from the thread's LAST comment (the most recent
	// point of the conversation); DiffHunk comes from the FIRST comment (the
	// anchor the thread was opened against). All present only for detail bodies.
	Author   string `json:"author,omitempty"`
	Body     string `json:"body,omitempty"`
	DiffHunk string `json:"diff_hunk,omitempty"`
}

// GeneralComment is a distilled, actionable general PR comment.
type GeneralComment struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// CommitSummary describes the head commit, including parsed co-authors.
type CommitSummary struct {
	Oid             string   `json:"oid"`
	ShortOid        string   `json:"short_oid"`
	Author          string   `json:"author"`
	Coauthors       []string `json:"coauthors,omitempty"`
	MessageHeadline string   `json:"message_headline"`
}

// PRStatus is the stable snapshot the change detector diffs.
type PRStatus struct {
	State             string           `json:"state"`
	Merged            bool             `json:"merged"`
	UnresolvedThreads []ThreadSummary  `json:"unresolved_threads"`
	GeneralComments   []GeneralComment `json:"general_comments"`
	Conflict          bool             `json:"conflict"`
	FailingChecks     []string         `json:"failing_checks"`
	PendingChecks     []string         `json:"pending_checks"`
	// SuccessfulChecks names the checks that finished without failing. It is
	// what separates "CI passed" from "CI has not reported yet" — both leave
	// FailingChecks and PendingChecks empty.
	SuccessfulChecks []string      `json:"successful_checks"`
	ReviewDecision   string        `json:"review_decision,omitempty"`
	ReviewAuthor     string        `json:"review_author,omitempty"`
	LastCommit       CommitSummary `json:"last_commit"`
	// CheckAnnotations holds the combined annotations from all completed
	// check runs (filtered to WARNING + FAILURE levels).
	CheckAnnotations []AnnotationSummary `json:"check_annotations,omitempty"`
}

// SnapshotOptions configures snapshot building.
type SnapshotOptions struct {
	// IgnoredBots are author logins whose general comments are dropped.
	IgnoredBots []string
}

// Snapshot distills a raw PR payload into a PRStatus.
//
// Filtering rules (ported from analyzer.ts):
//   - An unresolved thread is included only when it is not resolved AND its
//     last comment is not 👍-acknowledged.
//   - A general comment is included only when it is not 👍-acknowledged AND its
//     author is not in opts.IgnoredBots.
func Snapshot(pr *PullRequest, opts SnapshotOptions) *PRStatus {
	ignored := make(map[string]bool, len(opts.IgnoredBots))
	for _, b := range opts.IgnoredBots {
		ignored[b] = true
	}

	status := &PRStatus{
		State:             pr.State,
		Merged:            pr.Merged,
		Conflict:          pr.Mergeable == "CONFLICTING",
		UnresolvedThreads: []ThreadSummary{},
		GeneralComments:   []GeneralComment{},
		FailingChecks:     failingChecks(pr),
		PendingChecks:     pendingChecks(pr),
		SuccessfulChecks:  successfulChecks(pr),
		CheckAnnotations:  extractAnnotations(pr),
	}

	for _, t := range pr.ReviewThreads.Nodes {
		if t.IsResolved {
			continue
		}
		if last := lastComment(t.Comments.Nodes); last != nil && isAcknowledged(last) {
			continue
		}
		ids := make([]string, 0, len(t.Comments.Nodes))
		for i := range t.Comments.Nodes {
			ids = append(ids, t.Comments.Nodes[i].ID)
		}
		summary := ThreadSummary{
			ID:         t.ID,
			Path:       t.Path,
			Line:       t.Line,
			CommentIDs: ids,
		}
		if last := lastComment(t.Comments.Nodes); last != nil {
			summary.Author = last.Author.Login
			summary.Body = last.Body
		}
		if len(t.Comments.Nodes) > 0 {
			summary.DiffHunk = t.Comments.Nodes[0].DiffHunk
		}
		status.UnresolvedThreads = append(status.UnresolvedThreads, summary)
	}

	for i := range pr.Comments.Nodes {
		c := &pr.Comments.Nodes[i]
		if isAcknowledged(c) {
			continue
		}
		if ignored[c.Author.Login] {
			continue
		}
		status.GeneralComments = append(status.GeneralComments, GeneralComment{
			ID:     c.ID,
			Author: c.Author.Login,
			Body:   c.Body,
		})
	}

	status.ReviewDecision, status.ReviewAuthor = reviewDecision(pr)
	status.LastCommit = commitSummary(pr)

	return status
}

// ---------------------------------------------------------------------------
// Helpers / predicates
// ---------------------------------------------------------------------------

var failureConclusions = map[string]bool{
	"FAILURE": true, "ERROR": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true,
}

// successConclusions are the terminal conclusions that count as "this check
// passed" — SKIPPED and NEUTRAL are not failures and nothing more will happen
// to them, so they settle the check just as SUCCESS does.
var successConclusions = map[string]bool{
	"SUCCESS": true, "NEUTRAL": true, "SKIPPED": true,
}

// pendingStatuses covers every CheckStatusState except COMPLETED (plus the
// legacy STARTUP_FAILURE entry). A suite matching neither this map nor
// failureConclusions reads as settled, so omitting a non-terminal status here
// reports CI as passing while it is still queued.
var pendingStatuses = map[string]bool{
	"IN_PROGRESS": true, "QUEUED": true, "WAITING": true, "REQUESTED": true, "PENDING": true,
	"STARTUP_FAILURE": true,
}

var failureCommitStates = map[string]bool{"FAILURE": true, "ERROR": true}

var pendingCommitStates = map[string]bool{"PENDING": true, "EXPECTED": true}

var successCommitStates = map[string]bool{"SUCCESS": true}

func isFailureConclusion(c string) bool { return failureConclusions[c] }
func isSuccessConclusion(c string) bool { return successConclusions[c] }
func isPendingStatus(s string) bool     { return pendingStatuses[s] }

// acknowledgedReactions are the reaction contents that acknowledge a comment.
var acknowledgedReactions = map[string]bool{"THUMBS_UP": true}

// isAcknowledged reports whether a comment carries an acknowledging reaction.
func isAcknowledged(c *Comment) bool {
	for _, g := range c.ReactionGroups {
		if acknowledgedReactions[g.Content] && g.Users.TotalCount > 0 {
			return true
		}
	}
	return false
}

func lastComment(nodes []Comment) *Comment {
	if len(nodes) == 0 {
		return nil
	}
	return &nodes[len(nodes)-1]
}

// suiteName resolves a display name for a check suite.
func suiteName(s *CheckSuite) string {
	if s.App.Name != "" {
		return s.App.Name
	}
	return s.App.Slug
}

// extractAnnotations collects WARNING and FAILURE annotations from all
// check runs across all check suites of the head commit. The run status is
// deliberately not checked — an in-progress run may report partial
// annotations, but they are deduped on the next poll, so filtering on
// status gains nothing.
func extractAnnotations(pr *PullRequest) []AnnotationSummary {
	var out []AnnotationSummary
	seen := map[string]bool{}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			for _, run := range suite.CheckRuns.Nodes {
				for _, ann := range run.Annotations.Nodes {
					if !isAnnotationLevel(ann.Level) {
						continue
					}
					s := AnnotationSummary{
						CheckName: run.Name,
						Path:      ann.Path,
						Line:      ann.Location.Start.Line,
						Level:     ann.Level,
						Title:     ann.Title,
						Message:   ann.Message,
					}
					key := annotationKey(s)
					if !seen[key] {
						seen[key] = true
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

// failingChecks collects names of failing check suites/runs plus old-style
// status contexts in FAILURE/ERROR states.
func failingChecks(pr *PullRequest) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if isFailureConclusion(suite.Conclusion) {
				add(suiteName(suite))
			}
			for _, run := range suite.CheckRuns.Nodes {
				if isFailureConclusion(run.Conclusion) {
					name := run.Name
					if name == "" {
						name = suiteName(suite)
					}
					add(name)
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if failureCommitStates[ctx.State] {
					add(ctx.Context)
				}
			}
		}
	}
	return out
}

// successfulChecks collects names of check suites/runs that finished without
// failing, plus old-style status contexts in the SUCCESS state.
//
// This is the positive evidence that CI ran: failingChecks and pendingChecks
// are both empty whether every check passed or no check has been created yet,
// and only the former should be reported as green.
func successfulChecks(pr *PullRequest) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if isSuccessConclusion(suite.Conclusion) {
				add(suiteName(suite))
			}
			for _, run := range suite.CheckRuns.Nodes {
				if isSuccessConclusion(run.Conclusion) {
					name := run.Name
					if name == "" {
						name = suiteName(suite)
					}
					add(name)
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if successCommitStates[ctx.State] {
					add(ctx.Context)
				}
			}
		}
	}
	return out
}

// pendingChecks collects names of pending check suites plus old-style status
// contexts in PENDING/EXPECTED states.
func pendingChecks(pr *PullRequest) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if isPendingStatus(suite.Status) {
				add(suiteName(suite))
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if pendingCommitStates[ctx.State] {
					add(ctx.Context)
				}
			}
		}
	}
	return out
}

// nonDecisiveReviewStates are review states that do not constitute a review
// decision: PENDING (not yet submitted) and COMMENTED (comments only, neither
// approval nor a change request). Skipping them ensures a follow-up comment
// review does not clobber or misattribute an earlier APPROVED / CHANGES_REQUESTED
// decision.
var nonDecisiveReviewStates = map[string]bool{"PENDING": true, "COMMENTED": true}

// reviewDecision returns the state and author of the latest decisive review —
// the most recent review whose state is neither PENDING nor COMMENTED. Returns
// empty strings when there are no reviews or none are decisive.
func reviewDecision(pr *PullRequest) (state, author string) {
	nodes := pr.Reviews.Nodes
	for i := len(nodes) - 1; i >= 0; i-- {
		if !nonDecisiveReviewStates[nodes[i].State] {
			return nodes[i].State, nodes[i].Author.Login
		}
	}
	return "", ""
}

func commitSummary(pr *PullRequest) CommitSummary {
	if len(pr.Commits.Nodes) == 0 {
		return CommitSummary{}
	}
	c := pr.Commits.Nodes[0].Commit
	author := ""
	if len(c.Authors.Nodes) > 0 {
		a := c.Authors.Nodes[0]
		if a.User != nil && a.User.Login != "" {
			author = a.User.Login
		} else {
			author = a.Name
		}
	}
	short := c.Oid
	if len(short) > 7 {
		short = short[:7]
	}
	return CommitSummary{
		Oid:             c.Oid,
		ShortOid:        short,
		Author:          author,
		Coauthors:       parseCoauthors(c.Message),
		MessageHeadline: c.MessageHeadline,
	}
}

var coauthorRE = regexp.MustCompile(`(?im)^[ \t]*co-authored-by:[ \t]*(.+?)[ \t]*$`)
var trailingEmailRE = regexp.MustCompile(`[ \t]*<[^>]*>[ \t]*$`)

// parseCoauthors extracts co-author display names from Co-authored-by trailers
// in a commit message, stripping any trailing <email>, de-duplicated and in
// order of appearance. Returns nil when there are none.
func parseCoauthors(message string) []string {
	if message == "" {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, m := range coauthorRE.FindAllStringSubmatch(message, -1) {
		name := strings.TrimSpace(trailingEmailRE.ReplaceAllString(m[1], ""))
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// Ref snapshot types
// ---------------------------------------------------------------------------

// RefStatus is the stable snapshot for a ref/commit target.
type RefStatus struct {
	Oid             string   `json:"oid"`
	ShortOid        string   `json:"short_oid"`
	Author          string   `json:"author"`
	MessageHeadline string   `json:"message_headline"`
	FailingChecks   []string `json:"failing_checks"`
	PendingChecks   []string `json:"pending_checks"`
}

// SnapshotRef distills a RefTarget into a RefStatus for CI-only monitoring.
func SnapshotRef(ref *RefTarget) *RefStatus {
	status := &RefStatus{
		Oid: ref.Target.Oid,
	}
	if len(status.Oid) > 7 {
		status.ShortOid = status.Oid[:7]
	} else {
		status.ShortOid = status.Oid
	}
	status.MessageHeadline = ref.Target.MessageHeadline
	if len(ref.Target.Authors.Nodes) > 0 {
		a := ref.Target.Authors.Nodes[0]
		if a.User != nil && a.User.Login != "" {
			status.Author = a.User.Login
		} else {
			status.Author = a.Name
		}
	}
	status.FailingChecks = commitChecks(ref.Target.CheckSuites, ref.Target.Status, failingChecksFromCommit)
	status.PendingChecks = commitChecks(ref.Target.CheckSuites, ref.Target.Status, pendingChecksFromCommit)
	return status
}

// SnapshotCommit distills a CommitObject into a RefStatus for CI-only monitoring.
func SnapshotCommit(c *CommitObject) *RefStatus {
	target := RefTarget{}
	target.Target.Oid = c.Oid
	target.Target.MessageHeadline = c.MessageHeadline
	target.Target.Authors = c.Authors
	target.Target.CheckSuites = c.CheckSuites
	target.Target.Status = c.Status
	return SnapshotRef(&target)
}

// commitChecks extracts check names from check suites and status contexts
// using the provided classifier function which takes a *PullRequest.
func commitChecks(suites SuiteNodes, status *CommitStatus, classifier func(*PullRequest) []string) []string {
	pr := &PullRequest{
		Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
			CheckSuites: suites,
			Status:      status,
		}}}},
	}
	return classifier(pr)
}

// failingChecksFromCommit extracts failing check names from a synthetic PR.
func failingChecksFromCommit(pr *PullRequest) []string {
	return failingChecks(pr)
}

// pendingChecksFromCommit extracts pending check names from a synthetic PR.
func pendingChecksFromCommit(pr *PullRequest) []string {
	return pendingChecks(pr)
}

// ---------------------------------------------------------------------------
// Issue snapshot types
// ---------------------------------------------------------------------------

// IssueCommentSummary is a distilled, actionable issue comment.
type IssueCommentSummary struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// IssueStatus is the stable snapshot for an issue target.
type IssueStatus struct {
	State    string                `json:"state"`
	Title    string                `json:"title"`
	Comments []IssueCommentSummary `json:"comments"`
}

// ---------------------------------------------------------------------------
// Repo monitoring (watch a repository for new PRs and issues)
// ---------------------------------------------------------------------------

// MONITOR_REPO_QUERY fetches the most recently-created PRs and issues for a
// repository (up to 25 of each), ordered by creation date descending.
const MONITOR_REPO_QUERY = `query MonitorRepo($owner: String!, $repo: String!, $first: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequests(first: $first, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes {
        number
        title
        state
        url
        createdAt
        author { login }
      }
    }
    issues(first: $first, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes {
        number
        title
        state
        url
        createdAt
        author { login }
      }
    }
  }
}`

// RepoQueryResponse mirrors the GraphQL envelope for a repo monitoring query.
type RepoQueryResponse struct {
	Repository struct {
		PullRequests RepoPRNodes    `json:"pullRequests"`
		Issues       RepoIssueNodes `json:"issues"`
	} `json:"repository"`
}

// RepoPRNodes holds the list of repository PRs.
type RepoPRNodes struct {
	Nodes []RepoPR `json:"nodes"`
}

// RepoIssueNodes holds the list of repository issues.
type RepoIssueNodes struct {
	Nodes []RepoIssue `json:"nodes"`
}

// RepoPR is a single pull request from a repo listing.
type RepoPR struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
}

// RepoIssue is a single issue from a repo listing.
type RepoIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
}

// RepoItemSummary is a distilled repo item (PR or issue) used in events.
type RepoItemSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author"`
	URL    string `json:"url"`
}

// RepoStatus is the stable snapshot for a repo target.
type RepoStatus struct {
	PRs    []RepoItemSummary `json:"prs"`
	Issues []RepoItemSummary `json:"issues"`
}

// FetchRepo retrieves the monitoring snapshot for a repository.
func (s *Service) FetchRepo(owner, repo string) (*RepoQueryResponse, error) {
	var result RepoQueryResponse
	err := s.API.GraphQL(MONITOR_REPO_QUERY, map[string]interface{}{
		"owner": owner,
		"repo":  repo,
		"first": 25,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// SnapshotRepo distills a RepoQueryResponse into a RepoStatus.
func SnapshotRepo(resp *RepoQueryResponse) *RepoStatus {
	status := &RepoStatus{}
	for _, p := range resp.Repository.PullRequests.Nodes {
		status.PRs = append(status.PRs, RepoItemSummary{
			Number: p.Number,
			Title:  p.Title,
			Author: p.Author.Login,
			URL:    p.URL,
		})
	}
	for _, i := range resp.Repository.Issues.Nodes {
		status.Issues = append(status.Issues, RepoItemSummary{
			Number: i.Number,
			Title:  i.Title,
			Author: i.Author.Login,
			URL:    i.URL,
		})
	}
	return status
}

// SnapshotIssue distills an IssueNode into an IssueStatus.
func SnapshotIssue(issue *IssueNode, opts SnapshotOptions) *IssueStatus {
	ignored := make(map[string]bool, len(opts.IgnoredBots))
	for _, b := range opts.IgnoredBots {
		ignored[b] = true
	}

	status := &IssueStatus{
		State:    issue.State,
		Title:    issue.Title,
		Comments: []IssueCommentSummary{},
	}

	for i := range issue.Comments.Nodes {
		c := &issue.Comments.Nodes[i]
		// Reuse the same ack check: thumbs-up on a comment acknowledges it.
		if isAcknowledged(&Comment{ReactionGroups: c.ReactionGroups}) {
			continue
		}
		if ignored[c.Author.Login] {
			continue
		}
		status.Comments = append(status.Comments, IssueCommentSummary{
			ID:     c.ID,
			Author: c.Author.Login,
			Body:   c.Body,
		})
	}

	return status
}
