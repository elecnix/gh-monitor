package threads

import "github.com/elecnix/gh-monitor/backend"

// These types are the review-thread capability's contract, so they live in the
// public backend package where an out-of-tree backend can implement against
// them. They are aliased here because this package produces them.

type (
	// ListOptions configures list filtering.
	ListOptions = backend.ThreadListOptions
	// Thread is a normalized review thread payload for JSON output.
	Thread = backend.Thread
	// ActionOptions controls resolve/unresolve operations.
	ActionOptions = backend.ThreadRef
	// ActionResult captures the outcome of a resolve/unresolve mutation.
	ActionResult = backend.ThreadResolution
	// ThreadComment represents a single comment in a review thread.
	ThreadComment = backend.ThreadComment
	// ThreadWithComments represents a review thread with all comments.
	ThreadWithComments = backend.ThreadWithComments
)
