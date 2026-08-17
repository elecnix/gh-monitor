package draft

import "github.com/elecnix/gh-monitor/backend"

// These types are the draft-status capability's contract; see the backend
// package for why they live there.

type (
	// ActionOptions controls draft/ready operations.
	ActionOptions = backend.DraftRef
	// ActionResult captures the outcome of a draft/ready mutation.
	ActionResult = backend.DraftResult
	// DraftInfo contains information about a PR's draft status.
	DraftInfo = backend.DraftInfo
)
