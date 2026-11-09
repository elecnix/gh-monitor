package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/upgradewatch"
)

// replacedHarness points the watch's ticker and its replacement at test
// doubles, and restores both when the test ends.
func replacedHarness(t *testing.T) chan string {
	t.Helper()
	origInterval, origReplace := watchUpgradeInterval, replaceProcessFn
	t.Cleanup(func() { watchUpgradeInterval, replaceProcessFn = origInterval, origReplace })
	watchUpgradeInterval = 5 * time.Millisecond

	calls := make(chan string, 4)
	replaceProcessFn = func(path string) error {
		calls <- path
		return nil
	}
	return calls
}

// startWatcher begins the tick loop with a tracker already sampled against the
// binary the watch is running, so a test can change the file afterwards and
// know the change is genuinely later than the baseline.
func startWatcher(t *testing.T, path, resumeID string, stderr *bytes.Buffer) (context.CancelFunc, chan struct{}) {
	t.Helper()
	tracker, err := upgradewatch.New(path)
	if err != nil {
		t.Fatalf("upgradewatch.New(%s): %v", path, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchInstalledWithTracker(ctx, tracker, path, resumeID, stderr)
	}()
	return cancel, done
}

func writeBinary(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestWatchReplacesItselfWhenTheInstalledBinaryChanges is the core promise of
// issue #216: a resident watch becomes the new binary instead of serving the
// image it started with.
func TestWatchReplacesItselfWhenTheInstalledBinaryChanges(t *testing.T) {
	calls := replacedHarness(t)
	t.Setenv(watchReexecCountEnv, "0")
	t.Setenv(watchResumeIDEnv, "")

	path := filepath.Join(t.TempDir(), "gh-monitor")
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	writeBinary(t, path, "binary-v1", base)

	var stderr bytes.Buffer
	cancel, done := startWatcher(t, path, "resume-abc", &stderr)
	defer cancel()

	// The upgrade lands while the watch runs.
	writeBinary(t, path, "binary-v2-longer", base.Add(time.Minute))

	select {
	case got := <-calls:
		if got != path {
			t.Fatalf("replaced with %q, want the installed path %q", got, path)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the watch never replaced itself; stderr: %s", stderr.String())
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not return after replacing the process")
	}

	// The replacement must carry the watch's resume ID, or the new image
	// replays a first poll instead of resuming.
	if got := os.Getenv(watchResumeIDEnv); got != "resume-abc" {
		t.Fatalf("resume ID for the new image = %q, want %q", got, "resume-abc")
	}
	if n := os.Getenv(watchReexecCountEnv); n != "1" {
		t.Fatalf("restart count = %q, want 1", n)
	}
	if !strings.Contains(stderr.String(), "restarting this watch on the new one") {
		t.Fatalf("expected a replacement line on stderr, got: %s", stderr.String())
	}
}

// TestWatchKeepsItsBinaryWhenNothingChanged covers the expensive mistake: a
// watch that replaces itself for an unchanged binary would restart every tick.
func TestWatchKeepsItsBinaryWhenNothingChanged(t *testing.T) {
	calls := replacedHarness(t)
	t.Setenv(watchReexecCountEnv, "0")

	path := filepath.Join(t.TempDir(), "gh-monitor")
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	writeBinary(t, path, "binary-v1", base)

	var stderr bytes.Buffer
	cancel, done := startWatcher(t, path, "resume-abc", &stderr)
	defer cancel()
	select {
	case <-done:
	case <-time.After(120 * time.Millisecond):
		cancel()
		<-done
	}

	select {
	case got := <-calls:
		t.Fatalf("an unchanged binary triggered a replacement with %q", got)
	default:
	}
	if strings.Contains(stderr.String(), "restarting this watch") {
		t.Fatalf("unexpected replacement line: %s", stderr.String())
	}
}

// TestWatchCannotRelaunchInALoop checks both independent guards: the restart
// counter stops a watch that keeps being handed a new binary, and a change the
// watcher has already adopted is never acted on twice.
func TestWatchCannotRelaunchInALoop(t *testing.T) {
	t.Run("counter stops a watch that keeps changing", func(t *testing.T) {
		calls := replacedHarness(t)
		t.Setenv(watchReexecCountEnv, "3") // already at maxWatchReexecs

		path := filepath.Join(t.TempDir(), "gh-monitor")
		base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		writeBinary(t, path, "binary-v1", base)

		var stderr bytes.Buffer
		cancel, done := startWatcher(t, path, "resume-abc", &stderr)
		defer cancel()
		writeBinary(t, path, "binary-v2-longer", base.Add(time.Minute))

		select {
		case got := <-calls:
			t.Fatalf("replaced past the restart limit, with %q", got)
		case <-time.After(150 * time.Millisecond):
		}
		// The watcher must have given up and returned, not kept ticking.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("the watcher kept ticking after refusing to relaunch")
		}
	})

	t.Run("one adopted change is never repeated", func(t *testing.T) {
		calls := replacedHarness(t)
		t.Setenv(watchReexecCountEnv, "0")

		path := filepath.Join(t.TempDir(), "gh-monitor")
		base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		writeBinary(t, path, "binary-v1", base)

		var stderr bytes.Buffer
		cancel, done := startWatcher(t, path, "resume-abc", &stderr)
		defer cancel()
		writeBinary(t, path, "binary-v2-longer", base.Add(time.Minute))

		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("no replacement happened; stderr: %s", stderr.String())
		}
		<-done // the watcher returns after one replacement, so it cannot loop

		select {
		case got := <-calls:
			t.Fatalf("a second replacement fired for one change: %q", got)
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestWatchContinuityGuard pins which watches may replace themselves. A repo
// watch without a named instance has an in-memory-only cursor, so replacing it
// would replay or skip events; every other shape resumes exactly.
func TestWatchContinuityGuard(t *testing.T) {
	cases := []struct {
		name     string
		kind     backend.Kind
		instance string
		want     bool
	}{
		{"pull request", backend.KindPR, "", true},
		{"issue", backend.KindIssue, "", true},
		{"ref", backend.KindRef, "", true},
		{"repo, unnamed", backend.KindRepo, "", false},
		{"repo, named instance", backend.KindRepo, "nightly", true},
	}
	for _, c := range cases {
		if got := watchContinuitySafe(c.kind, c.instance); got != c.want {
			t.Errorf("%s: watchContinuitySafe(%q, %q) = %v, want %v", c.name, c.kind, c.instance, got, c.want)
		}
	}
}
