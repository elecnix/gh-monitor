package monitor

import "strconv"

// EventType is a stable identifier for a kind of detected change.
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

	// EventDegraded signals that an API surface (rest, graphql, or both)
	// could not be read. The previous snapshot is retained; no inferred
	// state replaces it. Rate-limit details are carried when known.
	EventDegraded EventType = "degraded"
)

// Event describes a single genuinely-new change between two snapshots. Only the
// fields relevant to Type are populated.
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

	// DegradedResetAt is an ISO 8601 timestamp of the rate-limit reset (when
	// known). When set, the caller should back off until this time.
	DegradedResetAt string `json:"degraded_reset_at,omitempty"`
}

// Diff returns the genuinely-new changes between prev and curr.
//
// First-poll semantics (documented decision): when prev is nil the call is a
// baseline — it establishes state and emits NO events. This mirrors pi's "first
// poll learns state silently" for the commit oid and review decision, and
// extends it to every kind so the first snapshot never spams pre-existing
// comments, threads, failing checks, or conflicts as if they were new. Callers
// that want to surface the current state on first poll should read the PRStatus
// directly rather than relying on Diff.
func Diff(prev *PRStatus, curr *PRStatus) []Event {
	return diffImpl(prev, curr, false)
}

// DiffRetrigger is like Diff but diffs the thread and general-comment portions
// against an EMPTY baseline, so every currently-open unresolved thread and
// general comment re-emits on each poll (the retriggerComments preference).
// Checks / CI-green / review / commit / state transitions still diff against the
// real prev so those don't spam. The prev==nil first poll stays silent.
func DiffRetrigger(prev *PRStatus, curr *PRStatus) []Event {
	return diffImpl(prev, curr, true)
}

func diffImpl(prev *PRStatus, curr *PRStatus, retrigger bool) []Event {
	if prev == nil || curr == nil {
		return nil
	}

	// Under retrigger, threads/comments diff against an empty baseline so open
	// items re-notify every poll; everything else still diffs against prev.
	threadPrev := prev.UnresolvedThreads
	commentPrev := prev.GeneralComments
	if retrigger {
		threadPrev = nil
		commentPrev = nil
	}

	var events []Event

	// Conflict newly appeared.
	if curr.Conflict && !prev.Conflict {
		events = append(events, Event{Type: EventConflict})
	}

	// Checks failing now that were not failing before.
	if newFailing := diffNewStrings(prev.FailingChecks, curr.FailingChecks); len(newFailing) > 0 {
		events = append(events, Event{Type: EventNewFailingChecks, Checks: newFailing})
	}

	// CI went fully green: prev had work in flight or failing, curr has none.
	prevHadWork := len(prev.FailingChecks) > 0 || len(prev.PendingChecks) > 0 || len(prev.AwaitingChecks) > 0 || prev.RulesetError != "" || prev.TruncatedSuites
	currClean := len(curr.FailingChecks) == 0 && len(curr.PendingChecks) == 0 && len(curr.AwaitingChecks) == 0 && curr.RulesetError == "" && !curr.TruncatedSuites
	if prevHadWork && currClean {
		events = append(events, Event{Type: EventCIAllGreen})
	}

	// New or updated unresolved threads.
	if newThreads := diffNewThreads(threadPrev, curr.UnresolvedThreads); len(newThreads) > 0 {
		events = append(events, Event{Type: EventNewUnresolvedThreads, Threads: newThreads})
	}

	// New general comments.
	if newComments := diffNewComments(commentPrev, curr.GeneralComments); len(newComments) > 0 {
		events = append(events, Event{Type: EventNewGeneralComments, Comments: newComments})
	}

	// Review decision transitions (mirrors analyzer.ts formatStatusUpdate).
	if curr.ReviewDecision != prev.ReviewDecision {
		switch {
		case curr.ReviewDecision == "APPROVED":
			events = append(events, Event{Type: EventReviewApproved, ReviewState: curr.ReviewDecision, ReviewAuthor: curr.ReviewAuthor})
		case curr.ReviewDecision == "CHANGES_REQUESTED":
			events = append(events, Event{Type: EventReviewChangesRequested, ReviewState: curr.ReviewDecision, ReviewAuthor: curr.ReviewAuthor})
		case (curr.ReviewDecision == "" || curr.ReviewDecision == "DISMISSED") &&
			prev.ReviewDecision != "" && prev.ReviewDecision != "PENDING" && prev.ReviewDecision != "DISMISSED":
			events = append(events, Event{Type: EventReviewDismissed, ReviewState: curr.ReviewDecision, ReviewAuthor: curr.ReviewAuthor})
		}
	}

	// New head commit.
	if curr.LastCommit.Oid != "" && curr.LastCommit.Oid != prev.LastCommit.Oid {
		commit := curr.LastCommit
		events = append(events, Event{Type: EventNewCommit, Commit: &commit})
	}

	// State transitions.
	if curr.Merged && !prev.Merged {
		events = append(events, Event{Type: EventMerged})
	} else if curr.State == "CLOSED" && prev.State != "CLOSED" {
		events = append(events, Event{Type: EventClosed})
	}

	// New check-run annotations.
	if newAnn := diffAnnotations(prev.CheckAnnotations, curr.CheckAnnotations); len(newAnn) > 0 {
		evt := Event{Type: EventCheckAnnotations, Annotations: newAnn}
		if curr.AnnotationsTruncated {
			evt.AnnotationsTruncated = true
			evt.AnnotationsURL = curr.AnnotationsURL
		}
		events = append(events, evt)
	}

	return events
}

// diffNewStrings returns items in curr not present in prev, preserving order.
func diffNewStrings(prev, curr []string) []string {
	seen := make(map[string]bool, len(prev))
	for _, p := range prev {
		seen[p] = true
	}
	var out []string
	for _, c := range curr {
		if !seen[c] {
			out = append(out, c)
		}
	}
	return out
}

// diffNewComments returns comments whose ids are absent from prev.
func diffNewComments(prev, curr []GeneralComment) []GeneralComment {
	seen := make(map[string]bool, len(prev))
	for _, p := range prev {
		seen[p.ID] = true
	}
	var out []GeneralComment
	for _, c := range curr {
		if !seen[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// diffNewThreads returns threads that are "new" per isThreadNew: a thread whose
// id is absent from prev, or that gained a comment id not seen in prev.
func diffNewThreads(prev, curr []ThreadSummary) []ThreadSummary {
	prevIDs := make(map[string]bool, len(prev))
	prevCommentIDs := make(map[string]map[string]bool, len(prev))
	for _, t := range prev {
		prevIDs[t.ID] = true
		ids := make(map[string]bool, len(t.CommentIDs))
		for _, cid := range t.CommentIDs {
			ids[cid] = true
		}
		prevCommentIDs[t.ID] = ids
	}
	var out []ThreadSummary
	for _, t := range curr {
		if isThreadNew(t, prevIDs, prevCommentIDs) {
			out = append(out, t)
		}
	}
	return out
}

// isThreadNew reports whether a thread has content not seen in the previous
// snapshot: it did not exist before, or one of its current comment ids is new.
func isThreadNew(thread ThreadSummary, prevIDs map[string]bool, prevCommentIDs map[string]map[string]bool) bool {
	if !prevIDs[thread.ID] {
		return true
	}
	prevCids, ok := prevCommentIDs[thread.ID]
	if !ok {
		return true
	}
	for _, cid := range thread.CommentIDs {
		if !prevCids[cid] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// DiffRef — diff two RefStatus snapshots (CI-only monitoring)
// ---------------------------------------------------------------------------

// DiffRef returns genuinely-new CI events between two ref snapshots.
// First-poll (prev == nil) is silent.
func DiffRef(prev *RefStatus, curr *RefStatus) []Event {
	if prev == nil || curr == nil {
		return nil
	}

	var events []Event

	// New failing checks.
	if newFailing := diffNewStrings(prev.FailingChecks, curr.FailingChecks); len(newFailing) > 0 {
		events = append(events, Event{Type: EventNewFailingChecks, Checks: newFailing})
	}

	// CI all green.
	prevHadWork := len(prev.FailingChecks) > 0 || len(prev.PendingChecks) > 0
	currClean := len(curr.FailingChecks) == 0 && len(curr.PendingChecks) == 0
	if prevHadWork && currClean {
		events = append(events, Event{Type: EventCIAllGreen})
	}

	// New commit (OID changed).
	if curr.Oid != "" && curr.Oid != prev.Oid {
		commit := CommitSummary{
			Oid:             curr.Oid,
			ShortOid:        curr.ShortOid,
			Author:          curr.Author,
			MessageHeadline: curr.MessageHeadline,
		}
		events = append(events, Event{Type: EventNewCommit, Commit: &commit})
	}

	return events
}

// ---------------------------------------------------------------------------
// DiffIssues — diff two IssueStatus snapshots
// ---------------------------------------------------------------------------

// DiffIssues returns genuinely-new issue events between two snapshots.
// First-poll (prev == nil) is silent.
func DiffIssues(prev *IssueStatus, curr *IssueStatus) []Event {
	if prev == nil || curr == nil {
		return nil
	}

	var events []Event

	// State transitions.
	if curr.State != prev.State {
		switch {
		case curr.State == "CLOSED" && prev.State == "OPEN":
			events = append(events, Event{Type: EventIssueClosed})
		case curr.State == "OPEN" && prev.State == "CLOSED":
			events = append(events, Event{Type: EventIssueReopened})
		}
	}

	// New comments (by ID, same pattern as PR comments).
	prevIDs := make(map[string]bool, len(prev.Comments))
	for _, c := range prev.Comments {
		prevIDs[c.ID] = true
	}
	var newComments []IssueCommentSummary
	for _, c := range curr.Comments {
		if !prevIDs[c.ID] {
			newComments = append(newComments, c)
		}
	}
	if len(newComments) > 0 {
		events = append(events, Event{Type: EventIssueNewComment, IssueComments: newComments})
	}

	return events
}

// ---------------------------------------------------------------------------
// DiffRun — diff two RunStatus snapshots (workflow-run monitoring)
// ---------------------------------------------------------------------------

// DiffRun returns genuinely-new workflow-run events between two snapshots.
// First-poll (prev == nil) is silent. The run reaches a terminal state when
// its status becomes "completed"; the conclusion (success, failure, timed_out,
// cancelled, neutral, action_required, stale, skipped) is carried on the
// run-completed event.
func DiffRun(prev, curr *RunStatus) []Event {
	if prev == nil || curr == nil {
		return nil
	}
	if prev.Status == curr.Status {
		return nil
	}
	switch curr.Status {
	case "completed":
		return []Event{{Type: EventRunCompleted, RunConclusion: curr.Conclusion}}
	case "in_progress":
		return []Event{{Type: EventRunInProgress}}
	case "queued":
		return []Event{{Type: EventRunQueued}}
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// DiffRepo — diff two RepoStatus snapshots (repo monitoring)
// ---------------------------------------------------------------------------

// DiffRepo returns genuinely-new PR and issue events between two repo snapshots.
// First-poll (prev == nil) is silent. Items are matched by number; anything in
// curr not present in prev is emitted as a new event (one event per item).
func DiffRepo(prev, curr *RepoStatus) []Event {
	if prev == nil || curr == nil {
		return nil
	}

	prevPRs := make(map[int]bool, len(prev.PRs))
	for _, p := range prev.PRs {
		prevPRs[p.Number] = true
	}
	prevIssues := make(map[int]bool, len(prev.Issues))
	for _, i := range prev.Issues {
		prevIssues[i.Number] = true
	}

	var events []Event
	for _, p := range curr.PRs {
		if !prevPRs[p.Number] {
			events = append(events, Event{Type: EventRepoNewPR, RepoItems: []RepoItemSummary{p}})
		}
	}
	for _, i := range curr.Issues {
		if !prevIssues[i.Number] {
			events = append(events, Event{Type: EventRepoNewIssue, RepoItems: []RepoItemSummary{i}})
		}
	}
	return events
}

// ---------------------------------------------------------------------------
// Annotation types and helpers
// ---------------------------------------------------------------------------

// AnnotationSummary is a distilled check-run annotation.
type AnnotationSummary struct {
	CheckName string `json:"check_name"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Message   string `json:"message"`
}

// diffAnnotations returns annotations in curr that are not in prev.
// Annotations are matched by (check_name, path, line, level, title, message).
// Level filtering is performed at snapshot time via AnnotationLevels; the
// diff itself treats every annotation in CheckAnnotations equally.
func diffAnnotations(prev, curr []AnnotationSummary) []AnnotationSummary {
	seen := make(map[string]bool, len(prev))
	for _, a := range prev {
		seen[annotationKey(a)] = true
	}
	var out []AnnotationSummary
	for _, a := range curr {
		if !seen[annotationKey(a)] {
			out = append(out, a)
		}
	}
	return out
}

// annotationKey returns a stable dedup key for an annotation: check,
// path, line, level, title, and message combined.
func annotationKey(a AnnotationSummary) string {
	return a.CheckName + "\x00" + a.Path + "\x00" + strconv.Itoa(a.Line) + "\x00" + a.Level + "\x00" + a.Title + "\x00" + a.Message
}
