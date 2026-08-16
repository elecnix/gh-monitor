package comments

import "github.com/elecnix/gh-monitor/backend"

// These types are the thread-reply capability's contract; see the backend
// package for why they live there.

type (
	// ReplyOptions contains the payload for replying to a review comment thread.
	ReplyOptions = backend.ReplyOptions
	// Reply represents the normalized GraphQL response after adding a thread reply.
	Reply = backend.Reply
)
