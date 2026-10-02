package report

import "github.com/elecnix/gh-monitor/backend"

// The review-summary capability's contract lives in the public backend package
// so an out-of-tree backend can implement against it; see the backend package
// for why. It is aliased here because this package produces it — and because
// every existing caller spells these `report.Report`, `report.StateApproved`,
// and so on.

type (
	// State is a pull request review state supported by the report command.
	State = backend.ReportState
	// Options controls data retrieval and shaping for the report.
	Options = backend.ReportOptions
	// Report is the serialized output structure for the report command.
	Report = backend.Report
	// ReportReview aggregates review data and associated thread comments.
	ReportReview = backend.ReportReview
	// ReportComment contains the shaped parent comment for a thread.
	ReportComment = backend.ReportComment
	// ThreadReply captures a reply within a thread.
	ThreadReply = backend.ThreadReply
)

const (
	StateApproved         = backend.ReportStateApproved
	StateChangesRequested = backend.ReportStateChangesRequested
	StateCommented        = backend.ReportStateCommented
	StateDismissed        = backend.ReportStateDismissed
	StatePending          = backend.ReportStatePending
)
