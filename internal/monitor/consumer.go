package monitor

import "github.com/elecnix/gh-monitor/backend"

// PRConsumer holds the per-consumer state for a PR monitor: its own baseline
// snapshot and no-change counter. It is reused by the in-process runPR loop
// and by the hub's per-consumer goroutines so that consumption by one consumer
// never suppresses delivery to another (gh-monitor issues #34, #32).
//
// All diffing and rendering happens against the consumer's own baseline; the
// only shared input is the fetched *PRStatus, which a shared poller produces
// once and broadcasts to every consumer.
type PRConsumer struct {
	opts     RunOptions
	prev     *PRStatus
	noChange int
}

// NewPRConsumer creates a consumer with an empty baseline. The first call to
// Consume mirrors runPR's first poll: it diffs against an empty PRStatus so
// all pre-existing issues surface immediately.
func NewPRConsumer(opts RunOptions) *PRConsumer {
	return &PRConsumer{opts: opts}
}

// NoChange reports the number of consecutive polls that produced no events for
// this consumer. It drives idleInterval's backoff.
func (c *PRConsumer) NoChange() int { return c.noChange }

// Snapshot returns the consumer's current baseline: the last PRStatus it
// consumed, or nil before the first poll. The daemon's upgrade handoff uses it
// to carry a watcher's baseline to the next daemon, so the watcher resumes
// diffing where it left off instead of replaying what it already reported.
func (c *PRConsumer) Snapshot() *PRStatus { return c.prev }

// RestoreBaseline sets the consumer's baseline to a previously-stored snapshot
// so the next Consume call diffs from that restored baseline instead of an
// empty one. This is the resume path for named instances (issue #32): a restart
// delivers only what changed since the stored snapshot, rather than replaying
// the full backlog or silently missing the gap.
func (c *PRConsumer) RestoreBaseline(snapshot *PRStatus) {
	c.prev = snapshot
}

// Consume diffs curr against the consumer's baseline and invokes emit for
// every genuinely-new change, mirroring runPR's per-poll behaviour. It returns
// terminal=true when the PR is merged or closed (the caller should stop
// polling this identity for this consumer).
//
// The caller is responsible for fetching curr; Consume never touches the
// network, which is what lets many consumers share a single fetch.
func (c *PRConsumer) Consume(curr *PRStatus, emit func(backend.Update)) (terminal bool) {
	diff := Diff
	if c.opts.Prefs.RetriggerComments {
		diff = DiffRetrigger
	}

	firstPoll := c.prev == nil
	terminalEmitted := false
	compare := c.prev
	if firstPoll {
		compare = &PRStatus{}
		emit(c.opts.update(curr, Event{Type: EventFirstPoll}))
	}
	// Shed surfaces keep their last-known values so a tier drop never reads
	// as "cleared" (see CarryForwardShed).
	CarryForwardShed(c.prev, curr)
	events := diff(compare, curr)
	for _, ev := range events {
		if firstPoll && ev.Type == EventNewCommit {
			continue // the agent just pushed the head commit
		}
		emit(c.opts.update(curr, ev))
		if ev.Type == EventMerged || ev.Type == EventClosed {
			terminalEmitted = true
		}
	}
	// On first poll the diff against an empty baseline surfaces pre-existing
	// issues (failing checks, conflicts, threads) but never fires ci-all-green
	// because prevHadWork is always false. Emit it explicitly when CI has
	// already finished green.
	if firstPoll && ciAllGreen(curr) {
		emit(c.opts.update(curr, Event{Type: EventCIAllGreen}))
	}
	if len(events) == 0 {
		c.noChange++
	} else {
		c.noChange = 0
	}
	c.prev = curr

	if curr.Merged || curr.State == "CLOSED" {
		if firstPoll && !terminalEmitted {
			typ := EventClosed
			if curr.Merged {
				typ = EventMerged
			}
			emit(c.opts.update(curr, Event{Type: typ}))
		}
		return true
	}
	return false
}
