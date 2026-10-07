package mux

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
)

// coverageWatch serves a continuous watch whose sub-daemon declares the
// coverage capability. The repository decides who serves it:
//
//   - uncovered: the hub polls it at the normal cadence;
//   - covered: the sub-daemon serves it, the hub does not poll it, and the
//     hub fetches it once only when the repository has been quiet for the
//     safety interval.
//
// A change of coverage moves the watch with one fetch. The watch keeps the
// last status it delivered and passes it to the new source as its baseline,
// so a change made in between still reaches the watcher and nothing is
// reported twice.
func (s RoutingSource) coverageWatch(ctx context.Context, t backend.Target, opts backend.WatchOptions) <-chan backend.Update {
	out := make(chan backend.Update, 16)
	w := &coverWatch{s: s, ctx: ctx, t: t, opts: opts, out: out}
	go func() {
		defer close(out)
		w.run()
	}()
	return out
}

type coverWatch struct {
	s    RoutingSource
	ctx  context.Context
	t    backend.Target
	opts backend.WatchOptions
	out  chan backend.Update

	baseline string // JSON of the last status delivered
	reported bool   // a first poll has been delivered
	forceHub bool   // the sub-daemon ended the watch; stay on the hub until coverage changes
}

// outcome of one serving phase.
type phase int

const (
	phaseMoved phase = iota // coverage changed; re-route
	phaseEnded              // the watch is over (terminal update, cancel, or failure)
	phaseDied               // the sub-daemon ended a live watch early
)

func (w *coverWatch) send(u backend.Update) bool {
	select {
	case w.out <- u:
		return true
	case <-w.ctx.Done():
		return false
	}
}

// deliver forwards u and tracks the baseline. It reports false when the
// watch is over.
func (w *coverWatch) deliver(u backend.Update, mode string) bool {
	if u.Event.Type == backend.EventFirstPoll {
		w.reported = true
		if u.Event.PollMode == "" {
			u.Event.PollMode = mode
		}
	}
	if u.Status != nil {
		if b, err := json.Marshal(u.Status); err == nil {
			w.baseline = string(b)
		}
	} else if len(u.RawStatus) > 0 {
		w.baseline = string(u.RawStatus)
	}
	if !w.send(u) {
		return false
	}
	return !u.Terminal
}

func (w *coverWatch) run() {
	for w.ctx.Err() == nil {
		changed := w.s.Reg.cov.Changed()
		p, covered, last, _ := w.s.Reg.coverageRoute(w.t)
		var res phase
		if covered && !w.forceHub {
			res = w.serveSubdaemon(p, last, changed)
		} else {
			res = w.serveHub(changed)
		}
		switch res {
		case phaseEnded:
			return
		case phaseDied:
			w.forceHub = true
		case phaseMoved:
			w.forceHub = false
		}
	}
}

// stillHere re-reads coverage and reports whether the watch belongs where it
// is. A change that moves another repository leaves this watch alone.
func (w *coverWatch) stillHere(onSubdaemon bool) bool {
	_, covered, _, _ := w.s.Reg.coverageRoute(w.t)
	if w.forceHub {
		return !onSubdaemon
	}
	return covered == onSubdaemon
}

func (w *coverWatch) serveHub(changed <-chan struct{}) phase {
	mode := fmt.Sprintf("polling every %ds, repository has no webhook coverage", int(w.s.HubInterval.Seconds()))
	hctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	o := w.opts
	o.Baseline = w.baseline
	ch, err := w.s.Fallback.Watch(hctx, w.t, o)
	if err != nil {
		w.send(noticeUpdate(w.t, fmt.Sprintf("⚠️ the hub could not serve %s (%v)", w.t, err)))
		return phaseEnded
	}
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return phaseEnded
			}
			if !w.deliver(u, mode) {
				return phaseEnded
			}
		case <-changed:
			changed = w.s.Reg.cov.Changed()
			if !w.stillHere(false) {
				return phaseMoved
			}
		case <-w.ctx.Done():
			return phaseEnded
		}
	}
}

func (w *coverWatch) serveSubdaemon(p *remote.Provider, last time.Time, changed <-chan struct{}) phase {
	mode := fmt.Sprintf("webhook via %s, %s", p.Name(), w.safetyText())

	// One fetch: the backlog on the first move, a catch-up against the
	// delivered baseline afterwards.
	if !w.fetchOnce(mode) {
		return phaseEnded
	}
	if w.ctx.Err() != nil {
		return phaseEnded
	}

	sctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	rest := w.opts
	rest.Baseline = w.baseline
	stream, err := p.Watch(sctx, w.t, rest)
	if err != nil {
		w.notice(fmt.Sprintf("⚠️ sub-daemon %s could not serve %s (%v); polling it through the hub instead", p.Name(), w.t, err))
		return phaseDied
	}
	if w.reported {
		stream = skipFirstPoll(sctx, stream)
	}

	lastSafety := time.Now()
	timer := time.NewTimer(w.safetyWait(last, lastSafety))
	if w.s.SafetyInterval <= 0 {
		timer.Stop()
	}
	defer timer.Stop()
	for {
		select {
		case u, ok := <-stream:
			if !ok {
				if w.ctx.Err() != nil {
					return phaseEnded
				}
				w.notice(fmt.Sprintf("⚠️ sub-daemon %s stopped serving %s; polling it through the hub instead", p.Name(), w.t))
				return phaseDied
			}
			if !w.deliver(u, mode) {
				return phaseEnded
			}
		case <-changed:
			changed = w.s.Reg.cov.Changed()
			if !w.stillHere(true) {
				return phaseMoved
			}
		case <-timer.C:
			_, lastEvent, ok := w.s.Reg.cov.Covered(w.t.Owner, w.t.Repo)
			if !ok {
				continue // a coverage change is on its way
			}
			if wait := w.safetyWait(lastEvent, lastSafety); wait > 0 {
				timer.Reset(wait)
				continue
			}
			// Quiet for the whole interval: read the target once, in case the
			// webhook stopped delivering or never carried the event we need.
			if !w.fetchOnce(mode) {
				return phaseEnded
			}
			lastSafety = time.Now()
			timer.Reset(w.s.SafetyInterval)
		case <-w.ctx.Done():
			return phaseEnded
		}
	}
}

// safetyWait is how long until the safety fetch is due: the interval after
// the later of the last event and the last fetch.
func (w *coverWatch) safetyWait(lastEvent, lastFetch time.Time) time.Duration {
	if w.s.SafetyInterval <= 0 {
		return 0
	}
	ref := lastEvent
	if lastFetch.After(ref) {
		ref = lastFetch
	}
	return max(time.Until(ref.Add(w.s.SafetyInterval)), 0)
}

func (w *coverWatch) safetyText() string {
	if w.s.SafetyInterval <= 0 {
		return "no safety check"
	}
	return "safety check every " + w.s.SafetyInterval.String()
}

// fetchOnce reads the target through the hub once against the baseline and
// delivers what changed. It reports false when the watch is over.
func (w *coverWatch) fetchOnce(mode string) bool {
	o := w.opts
	o.Once = true
	o.Timeout = 0
	o.Baseline = w.baseline
	ch, err := w.s.Fallback.Watch(w.ctx, w.t, o)
	if err != nil {
		w.notice(fmt.Sprintf("⚠️ could not fetch %s (%v); sub-daemon updates continue from the last known state", w.t, err))
		return true
	}
	for u := range ch {
		if !w.deliver(u, mode) {
			return false
		}
	}
	return true
}

// notice logs a routing diagnostic and tells the watcher.
func (w *coverWatch) notice(msg string) {
	_, _ = fmt.Fprintf(w.s.Reg.out, "gh-monitor daemon: %s\n", msg)
	w.send(noticeUpdate(w.t, msg))
}
