package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// watchForTest runs watchInstalledBinary with a short check interval and
// returns a stop function that cancels the watcher and waits for it to exit,
// so tests that retune the package vars it reads cannot race it.
func watchForTest(t *testing.T, installed, socket string, stderr func(string)) func() {
	t.Helper()
	origInterval := upgradeCheckInterval
	upgradeCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { upgradeCheckInterval = origInterval })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchInstalledBinary(ctx, installed, socket, time.Hour, writerFunc(stderr))
	}()
	return func() {
		cancel()
		<-done
	}
}

type writerFunc func(string)

func (f writerFunc) Write(p []byte) (int, error) {
	f(string(p))
	return len(p), nil
}

// TestUpgradeWatcherSpawnsOnBinaryChange is the trigger side of the seamless
// upgrade: when the installed binary changes on disk, the resident daemon
// spawns it as its successor (which performs the handoff).
func TestUpgradeWatcherSpawnsOnBinaryChange(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))

	var mu sync.Mutex
	var spawned []string
	origSpawn := spawnUpgradedDaemonFn
	t.Cleanup(func() { spawnUpgradedDaemonFn = origSpawn })
	spawnUpgradedDaemonFn = func(installed, socket string, interval time.Duration) error {
		mu.Lock()
		spawned = append(spawned, installed)
		mu.Unlock()
		return nil
	}
	var messages string
	stop := watchForTest(t, installed, filepath.Join(dir, "d.sock"), func(s string) { messages += s })
	t.Cleanup(stop)
	// Let the watcher record its baseline before the upgrade lands.
	time.Sleep(100 * time.Millisecond)

	// The upgrade lands: same path, new content.
	require.NoError(t, os.WriteFile(installed, []byte("v2-with-a-longer-body"), 0o755))

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(spawned)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("upgrade was never detected; messages=%q", messages)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestUpgradeWatcherQuietWhileUnchanged verifies an unchanged binary never
// triggers a spawn — the check must be a stat, not a restart.
func TestUpgradeWatcherQuietWhileUnchanged(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))

	var spawns int
	origSpawn := spawnUpgradedDaemonFn
	t.Cleanup(func() { spawnUpgradedDaemonFn = origSpawn })
	spawnUpgradedDaemonFn = func(installed, socket string, interval time.Duration) error {
		spawns++
		return nil
	}

	stop := watchForTest(t, installed, filepath.Join(dir, "d.sock"), func(string) {})
	time.Sleep(200 * time.Millisecond)
	stop()

	assert.Equal(t, 0, spawns, "an unchanged binary must not be spawned")
}

// TestUpgradeWatcherKeepsServingWhenSuccessorFails verifies a successor that
// cannot take over does not kill the resident daemon: the failure is logged
// and the watcher keeps running.
func TestUpgradeWatcherKeepsServingWhenSuccessorFails(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))

	origTakeover := upgradeTakeoverTimeout
	upgradeTakeoverTimeout = 50 * time.Millisecond
	t.Cleanup(func() { upgradeTakeoverTimeout = origTakeover })

	var messages string
	var mu sync.Mutex
	record := func(s string) { mu.Lock(); messages += s; mu.Unlock() }

	origSpawn := spawnUpgradedDaemonFn
	t.Cleanup(func() { spawnUpgradedDaemonFn = origSpawn })
	spawnUpgradedDaemonFn = func(installed, socket string, interval time.Duration) error {
		return nil // successor starts but never takes over
	}

	stop := watchForTest(t, installed, filepath.Join(dir, "d.sock"), record)
	t.Cleanup(stop)
	// Let the watcher record its baseline before the upgrade lands.
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, os.WriteFile(installed, []byte("v2-bigger"), 0o755))
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := messages
		mu.Unlock()
		if strings.Contains(got, "did not take over") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("failed takeover was never reported; messages=%q", got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestBuildUpgradeCommand pins the successor's invocation: same socket, same
// interval, daemon subcommand.
func TestBuildUpgradeCommand(t *testing.T) {
	cmd := buildUpgradeCommand("/path/gh-monitor", "/run/d.sock", 90*time.Second)
	require.NotNil(t, cmd)
	assert.Equal(t, "/path/gh-monitor", cmd.Path)
	assert.Equal(t, []string{"daemon", "--socket", "/run/d.sock", "--interval", "90"}, cmd.Args[1:])
}

// ---------------------------------------------------------------------------
// Self-update cadence (issue #69)
// ---------------------------------------------------------------------------

func TestSelfUpdateIntervalFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 0},                    // unset: off
		{"0", 0},                   // explicit off
		{"false", 0},               // explicit off
		{"1", time.Hour},           // on: default cadence
		{"true", time.Hour},        // on: default cadence
		{"30m", 30 * time.Minute},  // custom cadence
		{"2h", 2 * time.Hour},      // custom cadence
		{"garbage", 0},             // unparseable: off, never guess
		{"-5m", 0},                 // negative: off
	}
	for _, tc := range cases {
		t.Setenv(selfUpdateEnv, tc.env)
		assert.Equal(t, tc.want, selfUpdateIntervalFromEnv(), "GH_MONITOR_SELFUPDATE=%q", tc.env)
	}
}

// TestSelfUpdate_CheckerRunsUpgradeOnCadence verifies the loop end to end:
// with self-update enabled, the daemon runs `gh extension upgrade` on the
// configured cadence until cancelled.
func TestSelfUpdate_CheckerRunsUpgradeOnCadence(t *testing.T) {
	t.Setenv(reexec.InstalledBinEnv, "/some/installed/gh-monitor")

	var mu sync.Mutex
	checks := 0
	origUpgrade := extensionUpgradeFn
	t.Cleanup(func() { extensionUpgradeFn = origUpgrade })
	extensionUpgradeFn = func() error {
		mu.Lock()
		checks++
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checkForReleases(ctx, 5*time.Millisecond, writerFunc(func(string) {}))
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := checks
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected at least 3 upgrade checks, got %d", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestSelfUpdate_OffByDefault verifies that without GH_MONITOR_SELFUPDATE the
// checker never runs: a resident daemon must not shell out to `gh extension
// upgrade` unless the operator asked for it.
func TestSelfUpdate_OffByDefault(t *testing.T) {
	t.Setenv(selfUpdateEnv, "")
	t.Setenv(reexec.InstalledBinEnv, "/some/installed/gh-monitor")

	checks := 0
	origUpgrade := extensionUpgradeFn
	t.Cleanup(func() { extensionUpgradeFn = origUpgrade })
	extensionUpgradeFn = func() error {
		checks++
		return nil
	}

	cmd := newDaemonCommand()
	startSelfUpdate(context.Background(), cmd)
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, checks, "self-update must be opt-in")
}

// TestSelfUpdate_RequiresRuntimeCopyLaunch verifies the second gate: even
// when enabled, the checker does not run unless the daemon was launched from
// a runtime copy — otherwise the running image maps the installed file and an
// upgrade could not land anyway.
func TestSelfUpdate_RequiresRuntimeCopyLaunch(t *testing.T) {
	t.Setenv(selfUpdateEnv, "5ms")
	t.Setenv(reexec.InstalledBinEnv, "") // not launched via the runtime copy

	var logs []string
	cmd := newDaemonCommand()
	cmd.SetErr(writerFunc(func(s string) { logs = append(logs, s) }))

	checks := 0
	origUpgrade := extensionUpgradeFn
	t.Cleanup(func() { extensionUpgradeFn = origUpgrade })
	extensionUpgradeFn = func() error {
		checks++
		return nil
	}

	startSelfUpdate(context.Background(), cmd)
	time.Sleep(30 * time.Millisecond)
	assert.Zero(t, checks, "no runtime copy: self-update must stay off")
	require.NotEmpty(t, logs, "the disablement must be announced, never silent")
}

// TestSelfUpdate_FailedCheckIsNotFatal verifies a failed upgrade check is
// logged and retried, never fatal to the loop.
func TestSelfUpdate_FailedCheckIsNotFatal(t *testing.T) {
	t.Setenv(reexec.InstalledBinEnv, "/some/installed/gh-monitor")

	var mu sync.Mutex
	attempts := 0
	origUpgrade := extensionUpgradeFn
	t.Cleanup(func() { extensionUpgradeFn = origUpgrade })
	extensionUpgradeFn = func() error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			return fmt.Errorf("network down")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checkForReleases(ctx, 5*time.Millisecond, writerFunc(func(string) {}))
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 2 {
			break // the first failure did not stop the loop
		}
		select {
		case <-deadline:
			t.Fatal("the loop stopped after a failed check")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
