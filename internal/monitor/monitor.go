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
	"strconv"
	"strings"
	"time"

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

// ---------------------------------------------------------------------------
// Ruleset types (required status checks)
// ---------------------------------------------------------------------------

// RulesetListResponse is the minimal envelope for GET /repos/{owner}/{repo}/rulesets.
type RulesetListResponse []struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RulesetResponse is the minimal envelope for GET /repos/{owner}/{repo}/rulesets/{id}.
type RulesetResponse struct {
	ID     int           `json:"id"`
	Name   string        `json:"name"`
	Rules  []RulesetRule `json:"rules"`
	Target string        `json:"target"`
}

// RulesetRule is one rule inside a ruleset.
type RulesetRule struct {
	Type       string             `json:"type"`
	Parameters *RulesetParameters `json:"parameters,omitempty"`
}

// RulesetParameters holds the required_status_checks contexts.
type RulesetParameters struct {
	RequiredStatusChecks []RequiredStatusCheck `json:"required_status_checks"`
}

// RequiredStatusCheck is one required status context from a ruleset.
type RequiredStatusCheck struct {
	Context       string `json:"context"`
	IntegrationID int    `json:"integration_id"`
}

// RulesetChecks holds the result of fetching required checks from the ruleset.
// When Error is non-empty, the ruleset could not be read — the caller must
// degrade loudly rather than assuming nothing is required. When both Contexts
// and Error are empty, there are no required-checks rulesets (all clear).
type RulesetChecks struct {
	Contexts []string // required context names
	Error    string   // non-empty when the ruleset could not be read
}

// FetchRequiredChecks reads the branch ruleset for the repository and returns
// the required status check context names. It handles three cases:
//
//  1. No rulesets at all                     → Contexts=nil, Error=""
//  2. A ruleset with required_status_checks  → Contexts filled
//  3. The ruleset API returns 403/404        → Contexts=nil, Error="ruleset not readable"
//
// Case 3 is critical: a ruleset you cannot read must degrade loudly, never
// silently become "nothing is required" — that would reproduce the exact bug
// issue #30 is about.
func (s *Service) FetchRequiredChecks(owner, repo string) (*RulesetChecks, error) {
	// List rulesets.
	var list RulesetListResponse
	err := s.API.REST("GET", fmt.Sprintf("repos/%s/%s/rulesets", owner, repo), nil, nil, &list)
	if err != nil {
		// Degrade loudly: a fetch error (403/404 etc.) means we cannot
		// determine what is required. Return the error in the RulesetChecks
		// so the caller can surface it.
		return &RulesetChecks{
			Error: fmt.Sprintf("cannot read ruleset: %v", err),
		}, nil
	}

	var contexts []string
	for _, rs := range list {
		var detail RulesetResponse
		err := s.API.REST("GET", fmt.Sprintf("repos/%s/%s/rulesets/%d", owner, repo, rs.ID), nil, nil, &detail)
		if err != nil {
			return &RulesetChecks{
				Error: fmt.Sprintf("cannot read ruleset %d (%s): %v", rs.ID, rs.Name, err),
			}, nil
		}
		for _, rule := range detail.Rules {
			if rule.Type == "required_status_checks" && rule.Parameters != nil {
				for _, rc := range rule.Parameters.RequiredStatusChecks {
					contexts = append(contexts, rc.Context)
				}
			}
		}
	}

	return &RulesetChecks{Contexts: contexts}, nil
}

// RateLimitResource holds the core rate-limit info for one API category.
type RateLimitResource struct {
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	Reset     int64  `json:"reset"` // Unix epoch seconds
	ResetAt   string `json:"-"`     // ISO 8601 derived from Reset
}

// RateLimitResponse is the parsed response from GET /rate_limit.
type RateLimitResponse struct {
	Resources struct {
		Core    RateLimitResource `json:"core"`
		GraphQL RateLimitResource `json:"graphql"`
	} `json:"resources"`
}

// FetchRateLimit reads the current rate-limit status from GET /rate_limit.
func (s *Service) FetchRateLimit() (*RateLimitResponse, error) {
	var result RateLimitResponse
	if err := s.API.REST("GET", "rate_limit", nil, nil, &result); err != nil {
		return nil, err
	}
	if result.Resources.Core.Reset > 0 {
		result.Resources.Core.ResetAt = time.Unix(
			result.Resources.Core.Reset, 0).UTC().Format(time.RFC3339)
	}
	if result.Resources.GraphQL.Reset > 0 {
		result.Resources.GraphQL.ResetAt = time.Unix(
			result.Resources.GraphQL.Reset, 0).UTC().Format(time.RFC3339)
	}
	return &result, nil
}

// MONITOR_QUERY is kept for back-compat (tests and callers that snapshot a
// full PR); the tiered builder is MonitorQuery. See tier.go for the tier
// model and per-tier fragments.
var MONITOR_QUERY = MonitorQuery(TierFull)

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
	Nodes      []CheckSuite `json:"nodes"`
	TotalCount int          `json:"totalCount"`
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
	Nodes      []CheckRun `json:"nodes"`
	TotalCount int        `json:"totalCount"`
}

type CheckRun struct {
	Name        string          `json:"name"`
	Conclusion  string          `json:"conclusion"`
	Status      string          `json:"status"`
	StartedAt   string          `json:"startedAt"`
	CompletedAt string          `json:"completedAt"`
	DetailsURL  string          `json:"detailsUrl"`
	Permalink   string          `json:"permalink"`
	Annotations AnnotationNodes `json:"annotations"`
}

type AnnotationNodes struct {
	Nodes      []Annotation `json:"nodes"`
	TotalCount int          `json:"totalCount"`
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

// Fingerprint returns a stable string covering every field the change
// detector can fire an event on: state, mergeability, review decision, head
// commit, check outcomes (suites, runs, old-style status contexts), and the
// presence of comments, threads, and annotations. Two snapshots with equal
// fingerprints cannot produce a Diff event between them; an unequal
// fingerprint means a poll must not idle-back off.
//
// It is deliberately computed from identity fields (IDs, conclusions,
// decisions) rather than full bodies, matching what Diff actually compares:
// a comment body edit with the same ID produces no event today, so it does
// not count as a change here either.
func Fingerprint(pr *PullRequest) string {
	if pr == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(pr.State)
	b.WriteByte('|')
	b.WriteString(strconv.FormatBool(pr.Merged))
	b.WriteByte('|')
	b.WriteString(pr.Mergeable)
	b.WriteByte('|')
	b.WriteString(pr.MergeState)

	if len(pr.Commits.Nodes) > 0 {
		c := pr.Commits.Nodes[0].Commit
		b.WriteByte('|')
		b.WriteString(c.Oid)
		for i := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[i]
			b.WriteByte('|')
			b.WriteString(suite.App.Name)
			b.WriteByte(':')
			b.WriteString(suite.App.Slug)
			b.WriteByte(':')
			b.WriteString(suite.Conclusion)
			b.WriteByte(':')
			b.WriteString(suite.Status)
			for j := range suite.CheckRuns.Nodes {
				run := &suite.CheckRuns.Nodes[j]
				b.WriteByte(';')
				b.WriteString(run.Name)
				b.WriteByte(':')
				b.WriteString(run.Conclusion)
				b.WriteByte(':')
				b.WriteString(run.Status)
				for _, ann := range run.Annotations.Nodes {
					b.WriteByte(',')
					b.WriteString(ann.Path)
					b.WriteByte(':')
					b.WriteString(strconv.Itoa(ann.Location.Start.Line))
					b.WriteByte(':')
					b.WriteString(ann.Level)
					b.WriteByte(':')
					b.WriteString(ann.Title)
					b.WriteByte(':')
					b.WriteString(ann.Message)
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				b.WriteByte('!')
				b.WriteString(ctx.State)
				b.WriteByte(':')
				b.WriteString(ctx.Context)
			}
		}
	}

	for i := range pr.Comments.Nodes {
		b.WriteByte('#')
		b.WriteString(pr.Comments.Nodes[i].ID)
	}
	for i := range pr.ReviewThreads.Nodes {
		t := &pr.ReviewThreads.Nodes[i]
		b.WriteByte('~')
		b.WriteString(t.ID)
		b.WriteByte(':')
		b.WriteString(strconv.FormatBool(t.IsResolved))
		for j := range t.Comments.Nodes {
			b.WriteByte(',')
			b.WriteString(t.Comments.Nodes[j].ID)
		}
	}
	if state, author := reviewDecision(pr); state != "" {
		b.WriteByte('$')
		b.WriteString(state)
		b.WriteByte(':')
		b.WriteString(author)
	}
	return b.String()
}

// Fetch retrieves the full monitoring snapshot for a PR — equivalent to
// FetchWithTier with TierFull.
func (s *Service) Fetch(identity *resolver.Identity, number int) (*QueryResponse, error) {
	return s.FetchWithTier(identity, number, TierFull)
}

// FetchWithTier retrieves the monitoring snapshot for a PR at the given tier.
// Shedding surfaces keeps PR status + check outcomes alive under a tight
// GraphQL budget; see tier.go.
func (s *Service) FetchWithTier(identity *resolver.Identity, number int, tier QueryTier) (*QueryResponse, error) {
	var result QueryResponse
	err := s.API.GraphQL(MonitorQuery(tier), map[string]interface{}{
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
	return s.FetchRefWithTier(owner, repo, ref, TierFull)
}

// FetchRefWithTier retrieves the ref snapshot at the given tier. Ref queries
// carry no comments or reviews, so only annotations are shed.
func (s *Service) FetchRefWithTier(owner, repo, ref string, tier QueryTier) (*RefQueryResponse, error) {
	var result RefQueryResponse
	err := s.API.GraphQL(MonitorRefQuery(tier), map[string]interface{}{
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
	return s.FetchCommitWithTier(owner, repo, sha, TierFull)
}

// FetchCommitWithTier retrieves the commit snapshot at the given tier.
func (s *Service) FetchCommitWithTier(owner, repo, sha string, tier QueryTier) (*CommitQueryResponse, error) {
	var result CommitQueryResponse
	err := s.API.GraphQL(MonitorCommitQuery(tier), map[string]interface{}{
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
	// AwaitingChecks lists required status checks (from the branch ruleset)
	// that are entirely absent from the check-suites/status payload — they
	// have not been created yet. An absent required check is a distinct state
	// from QUEUED/IN_PROGRESS (which are in-flight but present in the
	// payload) and from FAILURE (which ran and failed). Only SKIPPED/NEUTRAL
	// on a required context counts as satisfied.
	AwaitingChecks []string `json:"awaiting_checks,omitempty"`
	// RulesetError is set when the ruleset could not be read (403/404/network
	// error). A non-empty RulesetError means the monitor cannot determine what
	// is required and degrades loudly rather than silently assuming nothing is
	// required. Empty when the fetch succeeded or there are no rulesets.
	RulesetError string `json:"ruleset_error,omitempty"`
	// TruncatedSuites is set when the checkSuites totalCount exceeds the
	// fetched node count — some suites were silently dropped by the page
	// limit. When true, the snapshot is degraded: AwaitingChecks may report
	// checks as absent that actually ran in the dropped suites.
	TruncatedSuites bool `json:"truncated_suites,omitempty"`
	// AnnotationsTruncated is true when the annotation set may be incomplete
	// because the API returned a truncated view (totalCount > fetched, or
	// totalCount >= 10 implying the per-step cap).
	AnnotationsTruncated bool `json:"annotations_truncated,omitempty"`
	// AnnotationsURL is the check run's permalink when annotations are
	// truncated, so a consumer can view the full set.
	AnnotationsURL string `json:"annotations_url,omitempty"`
	// ShedSurfaces names the surfaces the current tier does not fetch
	// (e.g. "annotations", "reviews"). The snapshot retains the last-known
	// values for those surfaces via CarryForwardShed, so a shed surface reads
	// as "not watched" rather than "cleared" — an APPROVED review must not
	// read as dismissed, failing checks must not read as CI-green. Empty when
	// nothing is shed. It is the loud part of degradation: consumers can see
	// exactly what is no longer being watched.
	ShedSurfaces []string `json:"shed_surfaces,omitempty"`
}

// SnapshotOptions configures snapshot building.
type SnapshotOptions struct {
	// IgnoredBots are author logins whose general comments are dropped.
	IgnoredBots []string

	// AnnotationLevels controls which check-run annotation levels are
	// included in the snapshot. A nil value uses the default (warning +
	// failure). An empty filter ("none") drops all annotations.
	AnnotationLevels *AnnotationLevels

	// RulesetChecks is the result of FetchRequiredChecks for the repo.
	// When nil, AwaitingChecks is not computed (the ruleset was not
	// fetched — e.g. ref/commit targets that lack a PR context).
	RulesetChecks *RulesetChecks

	// Tier is the query tier this snapshot was fetched at. It drives
	// ShedSurfaces on the resulting PRStatus so consumers can see what is no
	// longer being watched. TierFull (the zero value) sheds nothing.
	Tier QueryTier
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
		ShedSurfaces:      opts.Tier.ShedSurfaces(),
	}
	status.CheckAnnotations, status.AnnotationsTruncated, status.AnnotationsURL = extractAnnotations(pr, opts.AnnotationLevels)

	// Detect truncated suites: the API returned fewer nodes than exist.
	status.TruncatedSuites = truncatedSuites(pr)

	// Compute awaiting required checks from the ruleset.
	if opts.RulesetChecks != nil {
		status.RulesetError = opts.RulesetChecks.Error
		if opts.RulesetChecks.Error == "" && len(opts.RulesetChecks.Contexts) > 0 {
			status.AwaitingChecks = awaitingChecks(pr, opts.RulesetChecks.Contexts)
		}
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

// nonVerdictConclusions are terminal conclusions that are NOT results: the run was
// superseded or deliberately not executed, so it never overrides a verdict, whichever
// is newer. Measured 2026-08-18: a name carrying a cancelled run beside a successful
// one was reported as FAILING — the cancelled row was classified per-run instead of
// per name. The mirror trap (a newer skipped hiding an older failure) is the fleet's
// laundered-red case. One rule covers both signs: a non-verdict never overrides a
// verdict, in either direction.
var nonVerdictConclusions = map[string]bool{
	"SKIPPED": true, "CANCELLED": true, "STALE": true,
}

// runVerdict selects the newest VERDICT among a name's runs. A non-verdict
// (skipped/cancelled/stale) never overrides a verdict, whichever is newer; AMONG
// VERDICTS the latest wins, so a re-review that found a defect is the real verdict,
// not the earlier green. A run lacking a parseable completion time sorts oldest (it
// cannot prove it is newer), and document order breaks ties.
func runVerdict(runs []CheckRun) (CheckRun, bool) {
	var best CheckRun
	found := false
	var bestT time.Time
	for i := range runs {
		r := &runs[i]
		if r.Status != "COMPLETED" || nonVerdictConclusions[r.Conclusion] {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.CompletedAt)
		if err != nil {
			t = time.Time{} // unparseable cannot prove it is newer
		}
		if !found || t.After(bestT) {
			best = *r
			bestT = t
			found = true
		}
	}
	return best, found
}

// isFailureVerdict reports whether a VERDICT conclusion is a failure. CANCELLED is
// deliberately absent: it is a non-verdict (superseded attempt), used only as a
// fallback when the name has no verdict at all.
func isFailureVerdict(c string) bool {
	switch c {
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return true
	}
	return false
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

// extractAnnotations collects annotations from all check runs across all
// check suites of the head commit, filtered by levels. It also detects
// truncation: when any check run has totalCount >= 10 (the per-step GitHub
// cap) or totalCount > len(nodes) (our first: 50 page is full), the returned
// truncated flag is true and the URL points to the first such run's permalink.
func extractAnnotations(pr *PullRequest, levels *AnnotationLevels) (annotations []AnnotationSummary, truncated bool, url string) {
	var out []AnnotationSummary
	seen := map[string]bool{}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			for _, run := range suite.CheckRuns.Nodes {
				runAnns := run.Annotations
				if runAnns.TotalCount >= 10 && !truncated {
					truncated = true
					url = run.Permalink
				}
				if runAnns.TotalCount > len(runAnns.Nodes) && !truncated {
					truncated = true
					if url == "" {
						url = run.Permalink
					}
				}
				for _, ann := range runAnns.Nodes {
					if !levels.Allows(ann.Level) {
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
	return out, truncated, url
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
	// Per-name across ALL suites on the head: a cancelled run beside a successful one
	// for the same name is a superseded attempt, not a verdict. Classification is per
	// NAME across all runs, never per run (measured 2026-08-18).
	byName := map[string][]CheckRun{}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if isFailureConclusion(suite.Conclusion) {
				add(suiteName(suite))
			}
			for _, run := range suite.CheckRuns.Nodes {
				name := run.Name
				if name == "" {
					name = suiteName(suite)
				}
				if name != "" {
					byName[name] = append(byName[name], run)
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
	for name, runs := range byName {
		if v, ok := runVerdict(runs); ok {
			if isFailureVerdict(v.Conclusion) {
				add(name)
			}
			continue
		}
		// No verdict at all: fall back to any failure-shaped conclusion (a cancelled
		// suite that ran and was the only record still reads red, never green).
		for _, r := range runs {
			if isFailureConclusion(r.Conclusion) {
				add(name)
				break
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
	// Same per-name rule as failingChecks: the latest VERDICT decides. A name whose
	// latest verdict is a failure is NOT successful even if an earlier run passed.
	byName := map[string][]CheckRun{}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if isSuccessConclusion(suite.Conclusion) {
				add(suiteName(suite))
			}
			for _, run := range suite.CheckRuns.Nodes {
				name := run.Name
				if name == "" {
					name = suiteName(suite)
				}
				if name != "" {
					byName[name] = append(byName[name], run)
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
	for name, runs := range byName {
		if v, ok := runVerdict(runs); ok {
			if isSuccessConclusion(v.Conclusion) {
				add(name)
			}
			continue
		}
		for _, r := range runs {
			if isSuccessConclusion(r.Conclusion) {
				add(name)
				break
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

// truncatedSuites reports whether the check-suites payload was truncated —
// the API reported more suites than were returned in nodes. When true, the
// snapshot is degraded: AwaitingChecks may report checks as absent that
// actually ran in the dropped suites.
func truncatedSuites(pr *PullRequest) bool {
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		if c.CheckSuites.TotalCount > len(c.CheckSuites.Nodes) {
			return true
		}
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if suite.CheckRuns.TotalCount > len(suite.CheckRuns.Nodes) {
				return true
			}
		}
	}
	return false
}

// allPresentCheckNames collects every check name visible in the payload —
// suite names, individual run names, and old-style status context names.
func allPresentCheckNames(pr *PullRequest) map[string]bool {
	names := map[string]bool{}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		for j := range c.CheckSuites.Nodes {
			suite := &c.CheckSuites.Nodes[j]
			if sn := suiteName(suite); sn != "" {
				names[sn] = true
			}
			for _, run := range suite.CheckRuns.Nodes {
				if run.Name != "" {
					names[run.Name] = true
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if ctx.Context != "" {
					names[ctx.Context] = true
				}
			}
		}
	}
	return names
}

// awaitingChecks returns required context names that are entirely absent from
// the check-suites/status payload. A check that is present but not successful
// is still tracked by failingChecks / pendingChecks — awaiting means the check
// has not been created at all.
func awaitingChecks(pr *PullRequest, required []string) []string {
	present := allPresentCheckNames(pr)
	var out []string
	seen := map[string]bool{}
	for _, ctx := range required {
		if present[ctx] {
			continue
		}
		if !seen[ctx] {
			seen[ctx] = true
			out = append(out, ctx)
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

// isCommitOID reports whether s looks like a git commit OID: 7–40 hex
// characters. Seven is a plausible short SHA; forty is the full form.
func isCommitOID(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		isHex := false
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			isHex = true
		}
		if !isHex {
			return false
		}
	}
	return true
}

// ResolveRefBaseline turns a caller-supplied commit OID — one the caller
// observed directly, never one re-derived at watch time — into the JSON-ready
// RefStatus that seeds a ref watch's baseline (the --baseline flag). The OID
// is resolved through GitHub so a short SHA expands to the exact full form the
// API reports, check state at observation time is captured, and a typo'd or
// inaccessible SHA fails loudly instead of silently never matching.
func ResolveRefBaseline(api ghcli.API, owner, repo, raw string) (*RefStatus, error) {
	oid := strings.TrimSpace(raw)
	if !isCommitOID(oid) {
		return nil, fmt.Errorf("--baseline %q is not a valid commit OID (expected 7-40 hex characters); pass the full or short SHA you observed", raw)
	}
	svc := &Service{API: api}
	resp, err := svc.FetchCommit(owner, repo, oid)
	if err != nil {
		return nil, fmt.Errorf("--baseline: %w", err)
	}
	return SnapshotCommit(resp.Repository.Object), nil
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
