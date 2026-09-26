// Package orphan stops a resident gh-monitor watch whose launcher has exited.
//
// The monitor's contract is to poll until the target merges or closes, or
// until it is stopped explicitly. Nothing connected a watch to the process
// that started it, so a coding-agent session that exited — or a pane that
// closed, or a session that was replaced — left its monitor reparented and
// still polling (issue #129). Thirty such orphans share one GitHub API budget
// and starve the sessions that are still alive.
//
// The visible signature of a launcher that exited is a changed parent pid:
// the OS reassigns the child to a new parent (init, launchd, or the user
// service manager) when the old parent exits. A goroutine polls os.Getppid()
// and closes Done when it differs from the pid captured at start. Polling
// rather than prctl(PR_SET_PDEATHSIG, SIGTERM) is deliberate: PDEATHSIG
// observes the parent's main thread, which a multi-threaded launcher (Node,
// .NET) exits long before its process ends, so it would stop healthy watches.
// One integer read per poll is also the whole cost: no /proc parsing, and
// nothing platform-specific at all.
package orphan

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// OptOutEnv disables the orphan guard when set to "0". Every other value,
// including unset, keeps it on. It mirrors GH_MONITOR_REEXEC so an operator
// who opted out of one resident-process behaviour can reason about the other
// with the same convention.
const OptOutEnv = "GH_MONITOR_ORPHAN_GUARD"

// DefaultInterval is how often the guard re-reads the parent pid. It is short
// next to the watch's own minimum poll interval (10s), so detection never
// dominates the latency, and one integer read per tick costs nothing.
const DefaultInterval = 5 * time.Second

// Guard watches for the launcher's exit and reports it through Done.
type Guard struct {
	// ppid is the parent pid captured when polling started. It is written
	// once in Start and read by String, potentially from any goroutine, so
	// it is an atomic: the field has no other writer, and an atomic is the
	// cheapest thing that makes every read safe.
	ppid atomic.Int64
	// ppidFn returns the current parent pid. Tests inject their own so a
	// reparenting needs no real process.
	ppidFn func() int
	// interval is the poll cadence.
	interval time.Duration
	// done closes when the launcher is gone.
	done chan struct{}
	// stopc and stopOnce back Stop, which tests use to release the poll
	// goroutine. startOnce makes Start single-use. A production guard lives
	// exactly as long as its process and never calls Stop.
	stopOnce  sync.Once
	startOnce sync.Once
	stopc     chan struct{}
}

// New builds a guard that polls ppidFn at interval. It does not poll until
// Start; the indirection exists so a test can wire a fake poller before
// anything reads it.
func New(interval time.Duration, ppidFn func() int) *Guard {
	return &Guard{
		ppidFn:   ppidFn,
		interval: interval,
		done:     make(chan struct{}),
		stopc:    make(chan struct{}),
	}
}

// Start polls this process's real parent pid for its change.
func Start(interval time.Duration) *Guard {
	return New(interval, os.Getppid).Start()
}

// Start captures the parent pid and launches the poll loop. The first read
// is the baseline every later poll compares against. A guard is single-use:
// a second Start on the same guard is a no-op, not a second poll loop
// racing the first for the done channel.
func (g *Guard) Start() *Guard {
	g.startOnce.Do(func() {
		baseline := g.ppidFn()
		g.ppid.Store(int64(baseline))
		// A baseline of 0 is not a usable parent pid: the poll condition
		// (a different, non-zero parent) can then never be true, so the
		// guard would hold every watch forever. A failed read disables the
		// guard for this process — fail-safe in the direction of the
		// launcher still being present, never in the direction of a
		// permanently unwatchable process.
		if baseline == 0 {
			return
		}
		go func(ppid int) {
			ticker := time.NewTicker(g.interval)
			defer ticker.Stop()
			for {
				select {
				case <-g.stopc:
					return
				case <-ticker.C:
					if cur := g.ppidFn(); cur != ppid && cur != 0 {
						close(g.done)
						return
					}
				}
			}
		}(baseline)
	})
	return g
}

// Stop releases the poll goroutine. A production guard never calls it: its
// process exiting is the release. Tests call it so they leave no goroutine
// ticking. Like Done, it tolerates a nil guard, so a caller's defer needs no
// conditional.
func (g *Guard) Stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(func() { close(g.stopc) })
}

// Done closes when the launcher has exited (the parent pid changed). A nil
// guard's Done blocks forever, so a caller can select on it unconditionally:
// Done() on a nil *Guard is a nil channel receive, which never fires. That is
// what makes the disabled case free for callers.
func (g *Guard) Done() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.done
}

// envEnabled reports whether the guard is enabled: any value for
// GH_MONITOR_ORPHAN_GUARD except "0".
func envEnabled() bool {
	return os.Getenv(OptOutEnv) != "0"
}

// MaybeStart returns a started guard for this process, or nil when the guard
// is disabled. The nil is safe for callers: Done() on it never fires.
func MaybeStart() *Guard {
	if !envEnabled() {
		return nil
	}
	return Start(DefaultInterval)
}

// String renders the guard's baseline ppid for diagnostics.
func (g *Guard) String() string {
	if g == nil {
		return "guard disabled"
	}
	return "ppid " + fmt.Sprintf("%d", g.ppid.Load())
}
