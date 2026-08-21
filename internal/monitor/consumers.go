package monitor

import (
	"strconv"

	"github.com/elecnix/gh-monitor/backend"
)

// The consumers in this file hold the per-consumer state of one watch — its
// own baseline snapshot and no-change counter — so that consumption by one
// consumer never suppresses delivery to another (gh-monitor issues #34, #32).
//
// Each one is the single implementation of its target kind's per-poll
// behaviour: the shared poller's per-subscriber goroutines (internal/hub)
// drive exactly these Consume methods, and the in-process run loops drove
// them too before they were deleted — which is how parity was proven before
// the consolidation to a single watch code path (issue #76).
//
// Like PRConsumer, no consumer here ever touches the network: the caller
// fetches the current status and hands it in.

// ---------------------------------------------------------------------------
// Ref / commit target
// ---------------------------------------------------------------------------

// diffConsumer is the shared skeleton of the consumers whose per-poll
// behaviour is entirely: emit first-poll against an empty baseline, diff,
// emit each event, and track no-change polls. Ref/commit and repo targets
// both have exactly that shape (no terminal state, no snapshot persistence),
// so they are one generic implementation parameterized by status type and
// diff function — the duplicate-code detector reads this file, and so should
// you. Issue and run targets have extra per-poll behaviour (terminal states,
// snapshot saves, failed-log details) and keep their own consumers below.
type diffConsumer[S any] struct {
	opts     RunOptions
	prev     *S
	noChange int
	diff     func(prev, curr *S) []Event
}

// NoChange reports the number of consecutive polls that produced no events.
func (c *diffConsumer[S]) NoChange() int { return c.noChange }

// RestoreBaseline sets the consumer's baseline to a previously-stored
// snapshot.
func (c *diffConsumer[S]) RestoreBaseline(snapshot *S) { c.prev = snapshot }

// Consume diffs curr against the consumer's baseline and invokes emit for
// every genuinely-new change. It always returns false: these targets have no
// terminal state and run until cancelled or timed out.
func (c *diffConsumer[S]) Consume(curr *S, emit func(backend.Update)) bool {
	firstPoll := c.prev == nil
	// On the first poll, diff against an empty baseline so all pre-existing
	// state is surfaced immediately.
	compare := c.prev
	if firstPoll {
		compare = new(S)
		emit(c.opts.update(any(curr).(backend.Status), Event{Type: EventFirstPoll}))
	}
	events := c.diff(compare, curr)
	for _, ev := range events {
		emit(c.opts.update(any(curr).(backend.Status), ev))
	}
	if len(events) == 0 {
		c.noChange++
	} else {
		c.noChange = 0
	}
	c.prev = curr
	return false
}

// RefConsumer holds the per-consumer state for a ref or commit watch.
type RefConsumer = diffConsumer[RefStatus]

// NewRefConsumer creates a consumer with an empty baseline.
func NewRefConsumer(opts RunOptions) *RefConsumer {
	return &RefConsumer{opts: opts, diff: DiffRef}
}

// ---------------------------------------------------------------------------
// Issue target
// ---------------------------------------------------------------------------

// IssueConsumer holds the per-consumer state for an issue watch.
type IssueConsumer struct {
	opts     RunOptions
	prev     *IssueStatus
	noChange int
}

// NewIssueConsumer creates a consumer with an empty baseline.
func NewIssueConsumer(opts RunOptions) *IssueConsumer {
	return &IssueConsumer{opts: opts}
}

// NoChange reports the number of consecutive polls that produced no events.
func (c *IssueConsumer) NoChange() int { return c.noChange }

// RestoreBaseline sets the consumer's baseline to a previously-stored
// snapshot so the next Consume call diffs from that restored baseline instead
// of an empty one (the resume path for named instances, issue #32).
func (c *IssueConsumer) RestoreBaseline(snapshot *IssueStatus) { c.prev = snapshot }

// Consume diffs curr against the consumer's baseline and invokes emit for
// every genuinely-new change, matching the runIssue loop this consumer was
// extracted from,
// including persisting the baseline via opts.SaveSnapshot. It returns
// terminal=true when the issue is closed (a reopened issue continues).
func (c *IssueConsumer) Consume(curr *IssueStatus, emit func(backend.Update)) (terminal bool) {
	firstPoll := c.prev == nil
	terminalEmitted := false
	// On the first poll, diff against an empty baseline so all pre-existing
	// comments are surfaced immediately.
	compare := c.prev
	if firstPoll {
		compare = &IssueStatus{}
		emit(c.opts.update(curr, Event{Type: EventFirstPoll}))
	}
	events := DiffIssues(compare, curr)
	for _, ev := range events {
		emit(c.opts.update(curr, ev))
		if ev.Type == EventIssueClosed {
			terminalEmitted = true
		}
	}
	if len(events) == 0 {
		c.noChange++
	} else {
		c.noChange = 0
	}
	c.prev = curr
	saveIssueSnapshot(c.opts, curr)

	if curr.State == "CLOSED" {
		if firstPoll && !terminalEmitted {
			emit(c.opts.update(curr, Event{Type: EventIssueClosed}))
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Workflow-run target
// ---------------------------------------------------------------------------

// RunConsumer holds the per-consumer state for a workflow-run watch.
type RunConsumer struct {
	opts     RunOptions
	prev     *RunStatus
	noChange int

	// FailedLogDetail fetches the failed-run log snippet for a completed,
	// failed run. It is injected rather than owned because the fetch goes
	// through the gh CLI, whose client lives with the caller — the in-process
	// loop and the shared daemon wire different clients. When nil, failed-run
	// notifications carry no log snippet.
	FailedLogDetail func(runID int) string
}

// NewRunConsumer creates a consumer with an empty baseline.
func NewRunConsumer(opts RunOptions) *RunConsumer {
	return &RunConsumer{opts: opts}
}

// NoChange reports the number of consecutive polls that produced no events.
func (c *RunConsumer) NoChange() int { return c.noChange }

// RestoreBaseline sets the consumer's baseline to a previously-stored
// snapshot.
func (c *RunConsumer) RestoreBaseline(snapshot *RunStatus) { c.prev = snapshot }

// failedDetail attaches the failed-run log snippet to ev when it reports a
// completed run that did not succeed. runID identifies the run the detail is
// fetched for.
func (c *RunConsumer) failedDetail(ev Event, runID int) Event {
	if ev.Type == EventRunCompleted && isRunFailureConclusion(ev.RunConclusion) && c.FailedLogDetail != nil {
		ev.Detail = c.FailedLogDetail(runID)
	}
	return ev
}

// Consume diffs curr against the consumer's baseline and invokes emit for
// every genuinely-new change, matching the runRun loop this consumer was
// extracted from. It
// returns terminal=true once the run has completed (its conclusion rides the
// final run-completed event).
func (c *RunConsumer) Consume(curr *RunStatus, emit func(backend.Update)) (terminal bool) {
	firstPoll := c.prev == nil
	terminalEmitted := false
	// On the first poll, diff against an empty baseline so any pre-existing
	// state (e.g. a run that is already completed) is surfaced immediately.
	compare := c.prev
	if firstPoll {
		compare = &RunStatus{}
		emit(c.opts.update(curr, Event{Type: EventFirstPoll}))
	}
	events := DiffRun(compare, curr)
	for _, ev := range events {
		emit(c.opts.update(curr, c.failedDetail(ev, curr.RunID)))
		if ev.Type == EventRunCompleted {
			terminalEmitted = true
		}
	}
	if len(events) == 0 {
		c.noChange++
	} else {
		c.noChange = 0
	}
	c.prev = curr

	if curr.IsTerminal() {
		if firstPoll && !terminalEmitted {
			ev := Event{Type: EventRunCompleted, RunConclusion: curr.Conclusion}
			emit(c.opts.update(curr, c.failedDetail(ev, curr.RunID)))
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Repo target
// ---------------------------------------------------------------------------

// RepoConsumer holds the per-consumer state for a repository watch (new PRs
// and issues). It shares diffConsumer's skeleton with the ref/commit
// consumer; cursor advancement is the caller's concern: only the caller sees
// the raw response the distilled status came from, and the cursor must
// advance from every item in it — not just those this consumer emitted.
type RepoConsumer = diffConsumer[RepoStatus]

// NewRepoConsumer creates a consumer with an empty baseline.
func NewRepoConsumer(opts RunOptions) *RepoConsumer {
	return &RepoConsumer{opts: opts, diff: DiffRepo}
}

// ---------------------------------------------------------------------------
// Per-kind fingerprints
// ---------------------------------------------------------------------------

// FingerprintRef returns a stable string covering every field DiffRef can
// fire an event on: the head commit identity and the failing/pending check
// lists. Two snapshots with equal fingerprints cannot produce a DiffRef
// event between them.
func FingerprintRef(s *RefStatus) string {
	if s == nil {
		return ""
	}
	var b []byte
	b = append(b, s.Oid...)
	b = append(b, '|')
	for _, f := range s.FailingChecks {
		b = append(b, f...)
		b = append(b, ',')
	}
	b = append(b, '|')
	for _, p := range s.PendingChecks {
		b = append(b, p...)
		b = append(b, ',')
	}
	return string(b)
}

// FingerprintIssue returns a stable string covering every field DiffIssues
// can fire an event on: the issue state and the set of comment IDs.
func FingerprintIssue(s *IssueStatus) string {
	if s == nil {
		return ""
	}
	f := s.State + "|"
	for _, c := range s.Comments {
		f += c.ID + ","
	}
	return f
}

// FingerprintRun returns a stable string covering every field DiffRun can
// fire an event on: the run status and conclusion.
func FingerprintRun(s *RunStatus) string {
	if s == nil {
		return ""
	}
	return s.Status + "|" + s.Conclusion
}

// FingerprintRepo returns a stable string covering every field DiffRepo can
// fire an event on: the sets of PR and issue numbers.
func FingerprintRepo(s *RepoStatus) string {
	if s == nil {
		return ""
	}
	f := ""
	for _, p := range s.PRs {
		f += strconv.Itoa(p.Number) + ","
	}
	f += "|"
	for _, i := range s.Issues {
		f += strconv.Itoa(i.Number) + ","
	}
	return f
}
