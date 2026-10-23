package orphan

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestGuardExitsWhenPPIDChanges is the orphan-detection contract: the guard
// captures the parent pid it starts with, then a poll that observes a
// different parent pid closes Done. A changed ppid is the only visible
// signature of a launcher that exited while its child kept running.
func TestGuardExitsWhenPPIDChanges(t *testing.T) {
	// A guard wired to a stable poller never fires, however long it polls.
	// The atomic is what lets the test reparent the fake launcher while the
	// poll goroutine reads the same variable.
	var current atomic.Int64
	current.Store(4242)
	g := New(20*time.Millisecond, func() int { return int(current.Load()) }).Start()
	defer g.Stop()
	select {
	case <-g.Done():
		t.Fatal("guard fired while the parent pid stayed the same")
	case <-time.After(100 * time.Millisecond):
	}

	// Simulate reparenting: a launcher that exited leaves the child with a
	// parent pid it did not start with.
	current.Store(4243)
	select {
	case <-g.Done():
		// The expected path: the changed parent stops the watch.
	case <-time.After(2 * time.Second):
		t.Fatal("guard did not fire after the parent pid changed")
	}
}

// TestStartDoesNotFireWhileTheParentLives pins the real-process path: Start
// captures this test process's own parent (the go test driver) and a watch
// against it must hold for at least a couple of polls.
func TestStartDoesNotFireWhileTheParentLives(t *testing.T) {
	g := Start(20 * time.Millisecond)
	defer g.Stop()
	select {
	case <-g.Done():
		t.Fatal("guard fired while the real parent is still alive")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestNilGuardDoneNeverFires pins the disabled-case contract: a nil guard's
// Done() never fires, so a caller can select on it unconditionally.
func TestNilGuardDoneNeverFires(t *testing.T) {
	var g *Guard
	select {
	case <-g.Done():
		t.Fatal("a nil guard's Done must never fire")
	case <-time.After(20 * time.Millisecond):
	}
	assertEqual(t, "guard disabled", g.String())
}

// TestEnvEnabled pins the opt-out: GH_MONITOR_ORPHAN_GUARD=0 must disable the
// guard, and every other value (including unset) must keep it on.
func TestEnvEnabled(t *testing.T) {
	t.Setenv("GH_MONITOR_ORPHAN_GUARD", "0")
	if envEnabled() {
		t.Fatal("GH_MONITOR_ORPHAN_GUARD=0 must disable the guard")
	}
	t.Setenv("GH_MONITOR_ORPHAN_GUARD", "1")
	if !envEnabled() {
		t.Fatal("GH_MONITOR_ORPHAN_GUARD=1 must enable the guard")
	}
	t.Setenv("GH_MONITOR_ORPHAN_GUARD", "")
	if !envEnabled() {
		t.Fatal("an unset GH_MONITOR_ORPHAN_GUARD must enable the guard")
	}
}

// TestMaybeStartHonoursOptOut wires the public entry point to the env var.
func TestMaybeStartHonoursOptOut(t *testing.T) {
	t.Setenv("GH_MONITOR_ORPHAN_GUARD", "0")
	if g := MaybeStart(); g != nil {
		g.Stop()
		t.Fatal("MaybeStart must return nil when the guard is disabled")
	}
}

// assertEqual is a tiny local assertion keeping this package free of testify
// while the rest of the repo keeps its own conventions.
func assertEqual(t *testing.T, want, got string) {
	t.Helper()
	if want != got {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// TestNilGuardStopToleratesNil pins the deferred-call contract: a caller that
// wires `defer guard.Stop()` around a maybe-nil guard must not panic in the
// disabled case.
func TestNilGuardStopToleratesNil(t *testing.T) {
	var g *Guard
	g.Stop()
}

// TestStartIsSingleUse pins the single-use contract from review: a second
// Start on the same guard must not spawn a second poll loop that races the
// first for the done channel (a double close panics). One Start wins; a
// later reparenting is still detected by the surviving loop.
func TestStartIsSingleUse(t *testing.T) {
	var current atomic.Int64
	current.Store(4242)
	g := New(20*time.Millisecond, func() int { return int(current.Load()) })
	g.Start()
	g.Start() // must be a no-op, not a panic waiting to happen
	defer g.Stop()
	current.Store(4243)
	select {
	case <-g.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("guard did not fire after the parent pid changed")
	}
}

// TestZeroPPIDBaselineDisablesTheGuard pins the zero-baseline contract from
// review: a baseline read of 0 is not a usable parent pid (the poll
// condition could then never be true), so the guard disables itself rather
// than holding the watch forever.
func TestZeroPPIDBaselineDisablesTheGuard(t *testing.T) {
	g := New(20*time.Millisecond, func() int { return 0 }).Start()
	defer g.Stop()
	select {
	case <-g.Done():
		t.Fatal("a zero baseline must not produce a firing guard")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestNilPPIDFnDisablesTheGuard pins the nil-poller contract from review:
// New with a nil ppidFn must yield a disabled guard (Done never fires), not
// a panic on the first poll.
func TestNilPPIDFnDisablesTheGuard(t *testing.T) {
	g := New(20*time.Millisecond, nil).Start()
	defer g.Stop()
	select {
	case <-g.Done():
		t.Fatal("a nil ppidFn must not produce a firing guard")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestResolveLauncherWalksPastGh is the shape that leaked pollers: the watch's
// parent is `gh`, which forks the extension and waits, so `gh` is what gets
// reparented when the session exits — and watching the watch's own parent
// reported "launcher alive" forever.
func TestResolveLauncherWalksPastGh(t *testing.T) {
	// 100 = watch, 200 = gh (wrapper), 300 = the session that asked for it.
	tree := map[int]struct {
		ppid int
		comm string
	}{
		100: {200, "gh-monitor"},
		200: {300, "gh"},
		300: {1, "pi"},
	}
	parentOf := func(pid int) (int, string, bool) {
		e, ok := tree[pid]
		return e.ppid, e.comm, ok
	}
	if got := ResolveLauncher(100, parentOf); got != 300 {
		t.Fatalf("ResolveLauncher = %d, want the session 300 (not the wrapper)", got)
	}
}

// TestResolveLauncherDirectParent covers a direct invocation: the shell that
// started the watch is its launcher, resolved on the first hop — the guard's
// behaviour before wrappers were considered.
func TestResolveLauncherDirectParent(t *testing.T) {
	parentOf := func(pid int) (int, string, bool) {
		switch pid {
		case 100:
			return 50, "gh-monitor", true
		case 50:
			return 1, "bash", true
		}
		return 0, "", false
	}
	if got := ResolveLauncher(100, parentOf); got != 50 {
		t.Fatalf("ResolveLauncher = %d, want 50", got)
	}
}

func TestResolveLauncherUnknownIsZero(t *testing.T) {
	parentOf := func(int) (int, string, bool) { return 0, "", false }
	if got := ResolveLauncher(100, parentOf); got != 0 {
		t.Fatalf("ResolveLauncher = %d, want 0 when the tree is unreadable", got)
	}
	if got := ResolveLauncher(100, nil); got != 0 {
		t.Fatalf("ResolveLauncher with no reader = %d, want 0", got)
	}
}

// TestLauncherPIDIsReadable pins the fallback on this platform: the guard must
// always have a usable baseline pid, even when the walk finds nothing.
func TestLauncherPIDIsReadable(t *testing.T) {
	if got := LauncherPID(); got <= 0 {
		t.Fatalf("LauncherPID = %d, want a positive pid", got)
	}
}
