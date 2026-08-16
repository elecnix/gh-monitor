package review

import "github.com/elecnix/gh-monitor/backend"

// These types are the pending-review capability's contract; see the backend
// package for why they live there.

type (
	// ReviewState contains metadata about a review after opening or submitting it.
	ReviewState = backend.ReviewState
	// SubmitStatus represents the outcome of a review submission mutation.
	SubmitStatus = backend.ReviewSubmitStatus
	// ReviewThread represents an inline comment thread added to a pending review.
	ReviewThread = backend.ReviewThread
	// ThreadInput describes the inline comment details for AddThread.
	ThreadInput = backend.ReviewCommentInput
	// SubmitInput contains the payload for submitting a pending review.
	SubmitInput = backend.ReviewSubmitInput
	// UpdateCommentInput contains the payload for updating a comment in a pending review.
	UpdateCommentInput = backend.ReviewCommentUpdate
	// DeleteCommentInput contains the payload for deleting a comment in a pending review.
	DeleteCommentInput = backend.ReviewCommentDelete
)
