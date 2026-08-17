package backend

import (
	"context"
	"time"
)

// The mutation capabilities. Each is registered independently, so a backend
// can take over review threads without claiming to know how to submit a
// review, or serve drafts and nothing else.
const (
	// CapThreads means the backend lists, views, and resolves review threads.
	CapThreads Capability = "threads"
	// CapReview means the backend drives pending reviews.
	CapReview Capability = "review"
	// CapComments means the backend replies to review threads.
	CapComments Capability = "comments"
	// CapDraft means the backend reads and changes draft status.
	CapDraft Capability = "draft"
	// CapReactions means the backend adds reactions to nodes.
	CapReactions Capability = "reactions"
)

// ---------------------------------------------------------------------------
// Review threads
// ---------------------------------------------------------------------------

// ThreadActor lists, views, and resolves review threads.
type ThreadActor interface {
	ListThreads(ctx context.Context, t Target, opts ThreadListOptions) ([]Thread, error)
	ViewThreads(ctx context.Context, t Target, threadIDs []string) ([]ThreadWithComments, error)
	ResolveThread(ctx context.Context, t Target, ref ThreadRef) (ThreadResolution, error)
	UnresolveThread(ctx context.Context, t Target, ref ThreadRef) (ThreadResolution, error)
}

// ThreadListOptions configures list filtering.
type ThreadListOptions struct {
	OnlyUnresolved bool
	MineOnly       bool
}

// Thread is a normalized review thread payload for JSON output.
type Thread struct {
	ThreadID   string     `json:"thread_id"`
	IsResolved bool       `json:"is_resolved"`
	ResolvedBy *string    `json:"resolved_by,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	Path       string     `json:"path"`
	Line       *int       `json:"line,omitempty"`
	IsOutdated bool       `json:"is_outdated"`
}

// ThreadRef names the thread a resolve or unresolve applies to.
type ThreadRef struct {
	ThreadID string
}

// ThreadResolution is the outcome of a resolve/unresolve mutation.
type ThreadResolution struct {
	ThreadNodeID string `json:"thread_node_id"`
	IsResolved   bool   `json:"is_resolved"`
}

// ThreadComment is a single comment in a review thread.
type ThreadComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ThreadWithComments is a review thread with all of its comments.
type ThreadWithComments struct {
	ThreadID   string          `json:"thread_id"`
	IsResolved bool            `json:"is_resolved"`
	Path       string          `json:"path"`
	Line       *int            `json:"line,omitempty"`
	IsOutdated bool            `json:"is_outdated"`
	Comments   []ThreadComment `json:"comments"`
}

// ---------------------------------------------------------------------------
// Pending reviews
// ---------------------------------------------------------------------------

// ReviewActor drives a pending review from opening to submission.
type ReviewActor interface {
	StartReview(ctx context.Context, t Target, commitOID string) (*ReviewState, error)
	AddReviewComment(ctx context.Context, t Target, in ReviewCommentInput) (*ReviewThread, error)
	UpdateReviewComment(ctx context.Context, t Target, in ReviewCommentUpdate) error
	DeleteReviewComment(ctx context.Context, t Target, in ReviewCommentDelete) error
	SubmitReview(ctx context.Context, t Target, in ReviewSubmitInput) (*ReviewSubmitStatus, error)
}

// ReviewState is a review's metadata after opening or submitting it.
type ReviewState struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	SubmittedAt *string `json:"submitted_at,omitempty"`
}

// APIErrorEntry is one error the API returned alongside a response.
type APIErrorEntry struct {
	Message string        `json:"message"`
	Path    []interface{} `json:"path,omitempty"`
}

// ReviewSubmitStatus is the outcome of a review submission.
type ReviewSubmitStatus struct {
	Success bool
	Errors  []APIErrorEntry
}

// ReviewThread is an inline comment thread added to a pending review.
type ReviewThread struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	IsOutdated bool   `json:"is_outdated"`
	Line       *int   `json:"line,omitempty"`
}

// ReviewCommentInput describes an inline comment to add to a pending review.
type ReviewCommentInput struct {
	ReviewID  string
	Path      string
	Line      int
	Side      string
	StartLine *int
	StartSide *string
	Body      string
}

// ReviewSubmitInput is the payload for submitting a pending review.
type ReviewSubmitInput struct {
	ReviewID string
	Event    string
	Body     string
}

// ReviewCommentUpdate is the payload for editing a pending review comment.
type ReviewCommentUpdate struct {
	CommentID string
	Body      string
}

// ReviewCommentDelete is the payload for removing a pending review comment.
type ReviewCommentDelete struct {
	CommentID string
}

// ---------------------------------------------------------------------------
// Thread replies
// ---------------------------------------------------------------------------

// CommentActor replies to review threads.
type CommentActor interface {
	ReplyToThread(ctx context.Context, t Target, opts ReplyOptions) (Reply, error)
}

// ReplyOptions is the payload for replying to a review comment thread.
type ReplyOptions struct {
	ThreadID string
	ReviewID string
	Body     string
}

// Reply is the normalized response after adding a thread reply.
type Reply struct {
	CommentNodeID    string  `json:"comment_node_id"`
	DatabaseID       *int    `json:"database_id,omitempty"`
	ReviewID         *string `json:"review_id,omitempty"`
	ReviewDatabaseID *int    `json:"review_database_id,omitempty"`
	ReviewState      *string `json:"review_state,omitempty"`
	ThreadID         string  `json:"thread_id"`
	ThreadIsResolved bool    `json:"thread_is_resolved"`
	ThreadIsOutdated bool    `json:"thread_is_outdated"`
	ReplyToCommentID *string `json:"reply_to_comment_id,omitempty"`
	Body             string  `json:"body"`
	DiffHunk         *string `json:"diff_hunk,omitempty"`
	Path             string  `json:"path"`
	HtmlURL          string  `json:"html_url"`
	AuthorLogin      string  `json:"author_login"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Draft status
// ---------------------------------------------------------------------------

// DraftActor reads and changes a pull request's draft status.
type DraftActor interface {
	DraftStatus(ctx context.Context, t Target, ref DraftRef) (DraftInfo, error)
	SetDraft(ctx context.Context, t Target, ref DraftRef, draft bool) (DraftResult, error)
	ListDrafts(ctx context.Context, t Target) ([]DraftInfo, error)
}

// DraftRef overrides which pull request a draft operation applies to. A zero
// PRNumber means the one named by the Target.
type DraftRef struct {
	PRNumber int
}

// DraftResult is the outcome of a draft/ready mutation.
type DraftResult struct {
	PRNumber int    `json:"pr_number"`
	IsDraft  bool   `json:"is_draft"`
	Status   string `json:"status"`
}

// DraftInfo describes a pull request's draft status.
type DraftInfo struct {
	PRNumber int    `json:"pr_number"`
	IsDraft  bool   `json:"is_draft"`
	Title    string `json:"title"`
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

// ReactionActor adds a reaction to any node that accepts one.
type ReactionActor interface {
	React(ctx context.Context, t Target, subjectID, reaction string) error
}
