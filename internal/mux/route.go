package mux

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
)

// RoutingSource sends each watch to whichever source owns the target's kind:
// a live sub-daemon when one serves it, the fallback (the polling hub)
// otherwise. It is what lets event-driven sub-daemons and polled kinds
// coexist behind gh-monitor's single socket.
//
// A watch with a ResumeID routes like any other (issue #114). Every
// continuous CLI watch carries one, so excluding them kept sub-daemons from
// ever serving a watch. The ID travels to the sub-daemon; one that cannot
// resume starts the stream afresh after a daemon handoff, and the client's
// own reconnect notices cover the gap.
//
// A sub-daemon may know a target only from the events it saw since it
// started, so it cannot report what was posted before the watch (issue
// #119). The backlog therefore always comes from the fallback: a --once read
// is served by the fallback alone, and a continuous watch starts with one
// fallback fetch whose snapshot seeds the sub-daemon's baseline. The
// sub-daemon then serves the changes after it.
//
// A routed watch moves to the fallback in two cases, and says so in the
// stream each time:
//
//   - The dial fails, e.g. because the sub-daemon crashed a moment ago.
//   - The sub-daemon ends a continuous watch whose target is not done, e.g.
//     because it restarted or rejected the target.
//
// Hub polling keeps the target monitored in both cases. The notice tells the
// operator that the watch now spends the API budget the sub-daemon would
// have saved.
type RoutingSource struct {
	// Reg is the sub-daemon registry. Nil means "no sub-daemons configured";
	// every watch goes to the fallback unchanged.
	Reg *Registry
	// Fallback serves everything no live sub-daemon covers: the hubSource.
	Fallback backend.Source
}

// Watch implements backend.Source with the routing described above.
func (s RoutingSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	if s.Reg != nil {
		if p := s.Reg.Provider(t.Kind); p != nil && !opts.Once {
			ch := s.handOff(ctx, t, opts, p)
			if opts.Timeout > 0 {
				ch = relayWithTimeout(ctx, ch, opts.Timeout)
			}
			return ch, nil
		}
	}
	return s.fallback(ctx, t, opts, "")
}

// handOff serves a continuous watch in two parts. The fallback fetches the
// target once and delivers that first-poll batch, which is the backlog.
// Then the sub-daemon serves the rest of the watch, seeded with the
// snapshot the batch carried so it reports only what changed after it.
func (s RoutingSource) handOff(ctx context.Context, t backend.Target, opts backend.WatchOptions, p *remote.Provider) <-chan backend.Update {
	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		send := func(u backend.Update) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		forward := func(src <-chan backend.Update) {
			for u := range src {
				if !send(u) {
					return
				}
			}
		}

		first := opts
		first.Once = true
		first.Timeout = 0
		rest := opts
		reported := false
		backlog, err := s.Fallback.Watch(ctx, t, first)
		if err != nil {
			notice := fmt.Sprintf("⚠️ could not fetch the backlog of %s (%v); sub-daemon %s reports only later changes", t, err, p.Name())
			_, _ = fmt.Fprintf(s.Reg.out, "gh-monitor daemon: %s\n", notice)
			if !send(noticeUpdate(t, notice)) {
				return
			}
		} else {
			for u := range backlog {
				if !send(u) {
					return
				}
				if u.Terminal {
					return
				}
				if u.Event.Type == backend.EventFirstPoll {
					reported = true
				}
				if u.Status != nil {
					if b, err := json.Marshal(u.Status); err == nil {
						rest.Baseline = string(b)
					}
				}
			}
		}
		if ctx.Err() != nil {
			return
		}

		ch, err := p.Watch(ctx, t, rest)
		if err != nil {
			fb, err := s.fallback(ctx, t, rest, fmt.Sprintf(
				"⚠️ sub-daemon %s could not serve %s (%v); polling it through the hub instead", p.Name(), t, err))
			if err == nil {
				forward(fb)
			}
			return
		}
		if reported {
			ch = skipFirstPoll(ctx, ch)
		}
		forward(s.failover(ctx, t, rest, p.Name(), ch))
	}()
	return out
}

// skipFirstPoll drops the sub-daemon's first-poll batch when the fallback
// already reported the watch's first poll, so one watch reports one. A
// sub-daemon that starts with something else passes through unchanged.
func skipFirstPoll(ctx context.Context, in <-chan backend.Update) <-chan backend.Update {
	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		skipping, started := false, false
		for u := range in {
			if !started {
				started = true
				skipping = u.Event.Type == backend.EventFirstPoll && !u.Terminal
			}
			if skipping {
				skipping = u.More
				continue
			}
			select {
			case out <- u:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// fallback serves the watch from the hub. A non-empty notice is delivered
// first, as a degraded update, so the client learns why the hub serves it.
func (s RoutingSource) fallback(ctx context.Context, t backend.Target, opts backend.WatchOptions, notice string) (<-chan backend.Update, error) {
	ch, err := s.Fallback.Watch(ctx, t, opts)
	if err != nil {
		return nil, err
	}
	if notice != "" {
		_, _ = fmt.Fprintf(s.Reg.out, "gh-monitor daemon: %s\n", notice)
		ch = prepend(ctx, noticeUpdate(t, notice), ch)
	}
	if opts.Timeout > 0 {
		// The hub enforces Timeout itself, but a fallback that does not (or a
		// double relay) is harmless: the outer boundary closes first.
		ch = relayWithTimeout(ctx, ch, opts.Timeout)
	}
	return ch, nil
}

// failover relays a routed continuous watch and, when the sub-daemon ends it
// early, hands the rest of the watch to the fallback. The stream is finished,
// not broken, when the caller cancelled, a terminal update arrived, or the
// watch's own timeout has passed.
func (s RoutingSource) failover(ctx context.Context, t backend.Target, opts backend.WatchOptions, name string, in <-chan backend.Update) <-chan backend.Update {
	out := make(chan backend.Update, 16)
	started := time.Now()
	go func() {
		defer close(out)
		forward := func(src <-chan backend.Update) (terminal bool) {
			for u := range src {
				select {
				case out <- u:
				case <-ctx.Done():
					return false
				}
				if u.Terminal {
					return true
				}
			}
			return false
		}
		if forward(in) || ctx.Err() != nil {
			return
		}
		if opts.Timeout > 0 && time.Since(started) >= opts.Timeout {
			return
		}
		notice := fmt.Sprintf("⚠️ sub-daemon %s stopped serving %s; polling it through the hub instead", name, t)
		_, _ = fmt.Fprintf(s.Reg.out, "gh-monitor daemon: %s\n", notice)
		ch, err := s.Fallback.Watch(ctx, t, opts)
		if err != nil {
			notice = fmt.Sprintf("⚠️ sub-daemon %s stopped serving %s, and the hub could not take over (%v)", name, t, err)
		}
		select {
		case out <- noticeUpdate(t, notice):
		case <-ctx.Done():
			return
		}
		if err == nil {
			forward(ch)
		}
	}()
	return out
}

// noticeUpdate is a degraded update that carries a routing diagnostic.
func noticeUpdate(t backend.Target, notice string) backend.Update {
	return backend.Update{
		Target: t,
		Event: backend.Event{
			Type:   backend.EventDegraded,
			Notice: notice,
		},
		At: time.Now(),
	}
}

// prepend delivers first, then everything from in.
func prepend(ctx context.Context, first backend.Update, in <-chan backend.Update) <-chan backend.Update {
	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		select {
		case out <- first:
		case <-ctx.Done():
			return
		}
		for u := range in {
			select {
			case out <- u:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// relayWithTimeout stops the watch after timeout: the source channel closes
// when the timeout fires even if the backend itself would keep streaming —
// a client reading the channel sees a clean EOF either way. The hub enforces
// the same boundary for its own watches; routed watches need it here.
func relayWithTimeout(ctx context.Context, in <-chan backend.Update, timeout time.Duration) <-chan backend.Update {
	out := make(chan backend.Update, 16)
	timer := time.NewTimer(timeout)
	go func() {
		defer close(out)
		defer timer.Stop()
		for {
			select {
			case u, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- u:
				case <-timer.C:
					return
				case <-ctx.Done():
					return
				}
			case <-timer.C:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
