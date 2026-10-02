package backend

import "context"

// CapReport means the backend produces a structured review summary for a pull
// request: its reviews, their threads, and the comments in them.
const CapReport Capability = "report"

// ReportActor produces a structured review summary for a pull request.
//
// It is separate from ReviewActor on purpose. Driving a pending review is a
// mutation; producing a summary is a read, and a backend that can compute the
// answer from somewhere other than the GitHub GraphQL API — a cached index, a
// database of its own — has no reason to also know how to submit one.
type ReportActor interface {
	// ViewReport returns the shaped review summary for t.
	ViewReport(ctx context.Context, t Target, opts ReportOptions) (*Report, error)
}

// ReportState is one of the five states a GitHub review can be in. It is a
// string rather than an enum the CLI owns, so a backend that tracks its own
// states can still name them.
type ReportState string

const (
	ReportStateApproved         ReportState = "APPROVED"
	ReportStateChangesRequested ReportState = "CHANGES_REQUESTED"
	ReportStateCommented        ReportState = "COMMENTED"
	ReportStateDismissed        ReportState = "DISMISSED"
	ReportStatePending          ReportState = "PENDING"
)

// ReportOptions configures a review summary.
type ReportOptions struct {
	// Reviewer keeps only this author's reviews, case-insensitively.
	Reviewer string
	// States keeps only reviews in these states. Empty means every state.
	States []ReportState
	// StatesProvided distinguishes "no filter given" from an explicit empty
	// filter, which is what decides whether the state filter reaches the API
	// at all.
	StatesProvided bool
	// RequireUnresolved keeps only unresolved threads.
	RequireUnresolved bool
	// RequireNotOutdated keeps only current threads.
	RequireNotOutdated bool
	// TailReplies limits each thread to its last N replies; 0 means all.
	TailReplies int
	// IncludeCommentNodeID adds the GraphQL node IDs of parent comments and
	// replies to the output.
	IncludeCommentNodeID bool
	// Author keeps only threads containing a comment by this login,
	// case-insensitively.
	Author string
	// IncludeResolved keeps resolved threads even when RequireUnresolved is
	// set.
	IncludeResolved bool
}

// Report is a pull request's reviews with their thread comments.
//
// The types below are the capability's contract, so they live in this package
// where an out-of-tree backend can construct them. Their JSON tags are the
// output format: `gh monitor review view` emits exactly this.
type Report struct {
	Reviews []ReportReview `json:"reviews"`
}

// ReportReview is one review and the thread comments attached to it.
type ReportReview struct {
	ID          string          `json:"id"`
	State       ReportState     `json:"state"`
	Body        *string         `json:"body,omitempty"`
	SubmittedAt *string         `json:"submitted_at,omitempty"`
	AuthorLogin string          `json:"author_login"`
	Comments    []ReportComment `json:"comments,omitempty"`
}

// ReportComment is the parent comment of a review thread.
type ReportComment struct {
	ThreadID       string        `json:"thread_id"`
	CommentNodeID  *string       `json:"comment_node_id,omitempty"`
	Path           string        `json:"path"`
	Line           *int          `json:"line,omitempty"`
	AuthorLogin    string        `json:"author_login"`
	Body           string        `json:"body"`
	CreatedAt      string        `json:"created_at"`
	IsResolved     bool          `json:"is_resolved"`
	IsOutdated     bool          `json:"is_outdated"`
	ThreadComments []ThreadReply `json:"thread_comments"`
}

// ThreadReply is a reply within a review thread.
type ThreadReply struct {
	CommentNodeID *string `json:"comment_node_id,omitempty"`
	AuthorLogin   string  `json:"author_login"`
	Body          string  `json:"body"`
	CreatedAt     string  `json:"created_at"`
}
