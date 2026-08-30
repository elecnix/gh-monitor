package backend

import "strconv"

// EventType is a stable identifier for a kind of detected change. It is the
// vocabulary a Source speaks: whatever a backend learned, it says it in these
// terms, and the CLI renders and filters them uniformly.
type EventType string

const (
	EventNewFailingChecks       EventType = "new-failing-checks"
	EventCIAllGreen             EventType = "ci-all-green"
	EventNewUnresolvedThreads   EventType = "new-unresolved-threads"
	EventNewGeneralComments     EventType = "new-general-comments"
	EventConflict               EventType = "conflict"
	EventReviewApproved         EventType = "review-approved"
	EventReviewChangesRequested EventType = "review-changes-requested"
	EventReviewDismissed        EventType = "review-dismissed"
	EventNewCommit              EventType = "new-commit"
	EventMerged                 EventType = "merged"
	EventClosed                 EventType = "closed"

	// Issue monitoring events
	EventIssueClosed     EventType = "issue-closed"
	EventIssueReopened   EventType = "issue-reopened"
	EventIssueNewComment EventType = "issue-new-comment"
	EventIssueMention    EventType = "issue-mention"

	// Workflow-run monitoring events
	EventRunQueued     EventType = "run-queued"
	EventRunInProgress EventType = "run-in-progress"
	EventRunCompleted  EventType = "run-completed"

	// Repo monitoring events
	EventRepoNewPR        EventType = "repo-new-pr"
	EventRepoNewIssue     EventType = "repo-new-issue"
	EventCheckAnnotations EventType = "check-annotations"

	// Readiness view event (issue #31)
	EventRepoReadiness EventType = "readiness"

	// EventDegraded signals that an API surface (rest, graphql, or both)
	// could not be read. The previous snapshot is retained; no inferred
	// state replaces it. Rate-limit details are carried when known.
	EventDegraded EventType = "degraded"

	// EventFirstPoll is the baseline notification a Source emits when it
	// starts observing a target. It is not a change; it states what is being
	// watched so a silent watcher is distinguishable from a stalled one.
	EventFirstPoll EventType = "first-poll"

	// EventAllClear reports that every previously-raised issue is resolved.
	EventAllClear EventType = "all-clear"
)

// Event describes a single genuinely-new change on a Target. Only the fields
// relevant to Type are populated.
type Event struct {
	Type EventType `json:"type"`

	// Checks holds the newly-failing check names (EventNewFailingChecks).
	Checks []string `json:"checks,omitempty"`

	// Threads holds the new/updated unresolved threads (EventNewUnresolvedThreads).
	Threads []ThreadSummary `json:"threads,omitempty"`

	// Comments holds the new general comments (EventNewGeneralComments).
	Comments []GeneralComment `json:"comments,omitempty"`

	// ReviewState / ReviewAuthor describe a review transition
	// (EventReviewApproved / EventReviewChangesRequested / EventReviewDismissed).
	ReviewState  string `json:"review_state,omitempty"`
	ReviewAuthor string `json:"review_author,omitempty"`

	// Commit is the new head commit (EventNewCommit).
	Commit *CommitSummary `json:"commit,omitempty"`

	// IssueComments holds the new issue comments (EventIssueNewComment, EventIssueMention).
	IssueComments []IssueCommentSummary `json:"issue_comments,omitempty"`

	// RunConclusion is the terminal conclusion of a workflow run
	// (EventRunCompleted): success, failure, timed_out, cancelled, etc.
	RunConclusion string `json:"run_conclusion,omitempty"`

	// RepoItems holds the new PRs or issues (EventRepoNewPR, EventRepoNewIssue).
	RepoItems []RepoItemSummary `json:"repo_items,omitempty"`

	// Annotations holds the check-run annotations (EventCheckAnnotations).
	Annotations []AnnotationSummary `json:"annotations,omitempty"`

	// AnnotationsTruncated is true when the annotation set may be incomplete.
	AnnotationsTruncated bool `json:"annotations_truncated,omitempty"`

	// AnnotationsURL is the check run's permalink when annotations are
	// truncated, so a consumer can view the full set.
	AnnotationsURL string `json:"annotations_url,omitempty"`

	// DegradedSurface is set on EventDegraded to indicate which API surface
	// could not be read: "rest", "graphql", or "both".
	DegradedSurface string `json:"degraded_surface,omitempty"`

	// DegradedMessage carries the error detail for EventDegraded.
	DegradedMessage string `json:"degraded_message,omitempty"`

	// DegradedSurfaces names the WATCHED-SURFACE guarantees the degraded
	// read stopped delivering, as distinct from the API surface that failed
	// (DegradedSurface). It exists because a target's surfaces are coupled:
	// a PR's check outcomes, head commit, and mergeability ride the same
	// GraphQL query as its comments and reviews, so a failed PR query can
	// suppress check outcomes even though the tier system never sheds them
	// (issue #98). A caller that only knows "graphql failed" cannot tell
	// whether CI results are still trustworthy; one that knows "check
	// outcomes and head commit stopped arriving" can fall back to REST.
	// Empty for non-degraded events and for degraded surfaces that carry no
	// per-target state (e.g. a backend transport break).
	DegradedSurfaces []string `json:"degraded_surfaces,omitempty"`

	// DegradedResetAt is an ISO 8601 timestamp of the rate-limit reset (when
	// known). When set, the caller should back off until this time.
	DegradedResetAt string `json:"degraded_reset_at,omitempty"`

	// DegradedFrom is set on the RECOVERY notice of a degraded episode: an
	// RFC 3339 timestamp marking when the blind window opened — the last
	// successful observation before the failed fetch. DegradedTo is set on
	// the same notice: the instant recovery was confirmed. Together they
	// declare the gap (issue #99): events between the two were not observed,
	// and a cursor contract that never replays (cursors advance only on
	// successful fetches) means whatever happened in that window will not
	// be delivered later. A caller that needs completeness can re-read the
	// window from REST; a caller that does not know it has a hole cannot.
	// From is zero on the recovery notice only when the failure carried no
	// usable timestamp, never on a healthy notice.
	DegradedFrom string `json:"degraded_from,omitempty"`
	DegradedTo   string `json:"degraded_to,omitempty"`

	// Notice carries a fully-rendered message for diagnostics a Source raises
	// about itself rather than about the target — the built-in poller uses it
	// to say which surfaces it stopped watching under a tight API budget.
	// When set, the renderer uses it verbatim instead of a template, because
	// there is no target state to interpolate. A backend that has nothing of
	// the kind to report simply leaves it empty.
	Notice string `json:"notice,omitempty"`

	// Detail is an optional pre-rendered body for the event. The built-in
	// poller leaves it empty and lets the renderer build one from the
	// structured fields above; a backend that already has richer text may set
	// it directly.
	Detail string `json:"detail,omitempty"`
}

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

// CommitSummary describes a commit, including parsed co-authors.
type CommitSummary struct {
	Oid             string   `json:"oid"`
	ShortOid        string   `json:"short_oid"`
	Author          string   `json:"author"`
	Coauthors       []string `json:"coauthors,omitempty"`
	MessageHeadline string   `json:"message_headline"`
}

// IssueCommentSummary is a distilled, actionable issue comment.
type IssueCommentSummary struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// RepoItemSummary is a distilled repo item (PR or issue) used in events.
type RepoItemSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author"`
	URL    string `json:"url"`
}

// AnnotationSummary is a distilled check-run annotation.
type AnnotationSummary struct {
	CheckName string `json:"check_name"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Message   string `json:"message"`
}

// Key returns the stable dedup identity of an annotation — check, path, line,
// level, title, and message combined — used to detect which annotations are
// genuinely new between two observations.
func (a AnnotationSummary) Key() string {
	return a.CheckName + "\x00" + a.Path + "\x00" + strconv.Itoa(a.Line) + "\x00" +
		a.Level + "\x00" + a.Title + "\x00" + a.Message
}
