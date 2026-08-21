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
// behaviour: both the in-process run loops (run.go) and the shared poller's
// per-subscriber goroutines (internal/hub) drive the same Consume method, so
// the two code paths cannot drift apart — first-poll semantics, event
// vocabulary, and terminal states are identical by construction.
//
// Like PRConsumer, no consumer here ever touches the network: the caller
// fetches the current status and hands it in.

// ---------------------------------------------------------------------------
// Ref / commit target
// ---------------------------------------------------------------------------

// RefConsumer holds the per-consumer state for a ref or commit watch.
type RefConsumer struct {
	opts     RunOptions
	prev     *RefStatus
	noChange int
}

// NewRefConsumer creates a consumer with an empty baseline.
func NewRefConsumer(opts RunOptions) *RefConsumer {
	return &RefConsumer{opts: opts}
}

// NoChange reports the number of consecutive polls that produced no events.
func (c *RefConsumer) NoChange() int { return c.noChange }

// RestoreBaseline sets the consumer's baseline to a previously-stored
// snapshot.
func (c *RefConsumer) RestoreBaseline(snapshot *RefStatus) { c.prev = snapshot }

// Consume diffs curr against the consumer's baseline and invokes emit for
// every genuinely-new change, mirroring runRef's per-poll behaviour. Ref
// targets have no terminal state, so Consume always returns false.
func (c *RefConsumer) Consume(curr *RefStatus, emit func(backend.Update)) bool {
	firstPoll := c.prev == nil
	// On the first poll, diff against an empty baseline so all pre-existing
	// CI issues are surfaced immediately.
	compare := c.prev
	if firstPoll {
		compare = &RefStatus{}
		emit(c.opts.update(curr, Event{Type: EventFirstPoll}))
	}
	events := DiffRef(compare, curr)
	for _, ev := range events {
		emit(c.opts.update(curr, ev))
	}
	if len(events) == 0 {
		c.noChange++
	} else {
		c.noChange = 0
	}
	c.prev = curr
	return false
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
// every genuinely-new change, mirroring runIssue's per-poll behaviour,
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
// every genuinely-new change, mirroring runRun's per-poll behaviour. It
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
// and issues).
type RepoConsumer struct {
	opts     RunOptions
	prev     *RepoStatus
	noChange int
}

// NewRepoConsumer creates a consumer with an empty baseline.
func NewRepoConsumer(opts RunOptions) *RepoConsumer {
	return &RepoConsumer{opts: opts}
}

// NoChange reports the number of consecutive polls that produced no events.
func (c *RepoConsumer) NoChange() int { return c.noChange }

// RestoreBaseline sets the consumer's baseline to a previously-stored
// snapshot.
func (c *RepoConsumer) RestoreBaseline(snapshot *RepoStatus) { c.prev = snapshot }

// Consume diffs curr against the consumer's baseline and invokes emit for
// every genuinely-new change, mirroring runRepo's per-poll behaviour. Repo
// targets never reach a terminal state, so Consume always returns false.
// Cursor advancement is the caller's concern: only the caller sees the raw
// response the distilled status came from, and the cursor must advance from
// every item in it — not just those this consumer emitted.
func (c *RepoConsumer) Consume(curr *RepoStatus, emit func(backend.Update)) bool {
	firstPoll := c.prev == nil
	// On the first poll, diff against an empty baseline so all pre-existing
	// PRs and issues are surfaced immediately — but only those that passed
	// the caller's cursor filter (a new instance with no cursor shows
	// nothing).
	compare := c.prev
	if firstPoll {
		compare = &RepoStatus{}
		emit(c.opts.update(curr, Event{Type: EventFirstPoll}))
	}
	events := DiffRepo(compare, curr)
	for _, ev := range events {
		emit(c.opts.update(curr, ev))
	}
	if len(events) == 0 {
		c.noChange++
	} else {
		c.noChange = 0
	}
	c.prev = curr
	return false
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
