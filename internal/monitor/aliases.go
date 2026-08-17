package monitor

import "github.com/elecnix/gh-monitor/backend"

// The event vocabulary and the summary types an Event carries are the
// backend-facing contract, so they live in the public backend package where
// an out-of-tree backend can construct them. They are aliased here because
// this package is where they are produced, and because every existing caller
// spells them `monitor.Event`, `monitor.ThreadSummary`, and so on.

type (
	// EventType is a stable identifier for a kind of detected change.
	EventType = backend.EventType
	// Event describes a single genuinely-new change between two snapshots.
	Event = backend.Event

	// ThreadSummary is a distilled unresolved review thread.
	ThreadSummary = backend.ThreadSummary
	// GeneralComment is a distilled, actionable general PR comment.
	GeneralComment = backend.GeneralComment
	// CommitSummary describes a commit, including parsed co-authors.
	CommitSummary = backend.CommitSummary
	// IssueCommentSummary is a distilled, actionable issue comment.
	IssueCommentSummary = backend.IssueCommentSummary
	// RepoItemSummary is a distilled repo item (PR or issue) used in events.
	RepoItemSummary = backend.RepoItemSummary
	// AnnotationSummary is a distilled check-run annotation.
	AnnotationSummary = backend.AnnotationSummary
)

const (
	EventNewFailingChecks       = backend.EventNewFailingChecks
	EventCIAllGreen             = backend.EventCIAllGreen
	EventNewUnresolvedThreads   = backend.EventNewUnresolvedThreads
	EventNewGeneralComments     = backend.EventNewGeneralComments
	EventConflict               = backend.EventConflict
	EventReviewApproved         = backend.EventReviewApproved
	EventReviewChangesRequested = backend.EventReviewChangesRequested
	EventReviewDismissed        = backend.EventReviewDismissed
	EventNewCommit              = backend.EventNewCommit
	EventMerged                 = backend.EventMerged
	EventClosed                 = backend.EventClosed

	EventIssueClosed     = backend.EventIssueClosed
	EventIssueReopened   = backend.EventIssueReopened
	EventIssueNewComment = backend.EventIssueNewComment
	EventIssueMention    = backend.EventIssueMention

	EventRunQueued     = backend.EventRunQueued
	EventRunInProgress = backend.EventRunInProgress
	EventRunCompleted  = backend.EventRunCompleted

	EventRepoNewPR        = backend.EventRepoNewPR
	EventRepoNewIssue     = backend.EventRepoNewIssue
	EventCheckAnnotations = backend.EventCheckAnnotations

	EventRepoReadiness = backend.EventRepoReadiness
	EventDegraded      = backend.EventDegraded

	// EventFirstPoll and EventAllClear are loop-level kinds: they are not
	// produced by Diff but are emitted by the watch loop and do have prefs
	// templates.
	EventFirstPoll = backend.EventFirstPoll
	EventAllClear  = backend.EventAllClear
)
