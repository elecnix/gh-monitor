package mux

import (
	"context"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// RoutingSource sends each watch to whichever source owns the target's kind:
// a live sub-daemon when one serves it, the fallback (the polling hub)
// otherwise. It is what lets event-driven sub-daemons and polled kinds
// coexist behind gh-monitor's single socket.
//
// Two kinds of watches always go to the fallback:
//
//   - Resumable watches. A ResumeID names history held by the shared poller
//     (across upgrade handoffs); a sub-daemon has none of it.
//   - Watches whose routed dial fails, e.g. because the sub-daemon crashed a
//     moment ago. Falling back to hub polling keeps the target monitored —
//     degraded-but-covered instead of dead — until the registry's next probe
//     restores the route or confirms the loss.
type RoutingSource struct {
	// Reg is the sub-daemon registry. Nil means "no sub-daemons configured";
	// every watch goes to the fallback unchanged.
	Reg *Registry
	// Fallback serves everything no live sub-daemon covers: the hubSource.
	Fallback backend.Source
}

// Watch implements backend.Source with the routing described above.
func (s RoutingSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	if s.Reg != nil && opts.ResumeID == "" {
		if p := s.Reg.Provider(t.Kind); p != nil {
			ch, err := p.Watch(ctx, t, opts)
			if err == nil {
				if opts.Timeout > 0 {
					ch = relayWithTimeout(ctx, ch, opts.Timeout)
				}
				return ch, nil
			}
			// The routed dial failed — the sub-daemon may have just died.
			// Fall through to the fallback rather than fail the watch.
		}
	}
	ch, err := s.Fallback.Watch(ctx, t, opts)
	if err == nil && opts.Timeout > 0 {
		// The hub enforces Timeout itself, but a fallback that does not (or a
		// double relay) is harmless: the outer boundary closes first.
		ch = relayWithTimeout(ctx, ch, opts.Timeout)
	}
	return ch, err
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
