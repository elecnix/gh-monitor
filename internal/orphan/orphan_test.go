package orphan

import (
	"testing"
	"time"
)

// TestGuardExitsWhenPPIDChanges is the orphan-detection contract: the guard
// captures the parent pid it starts with, then a poll that observes a
// different parent pid closes Done. A changed ppid is the only visible
// signature of a launcher that exited while its child kept running.
func TestGuardExitsWhenPPIDChanges(t *testing.T) {
	// A guard wired to a stable poller never fires, however long it polls.
	current := 4242
	g := New(20*time.Millisecond, func() int { return current }).Start()
	defer g.Stop()
	select {
	case <-g.Done():
		t.Fatal("guard fired while the parent pid stayed the same")
	case <-time.After(100 * time.Millisecond):
	}

	// Simulate reparenting: a launcher that exited leaves the child with a
	// parent pid it did not start with.
	current = 4243
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
