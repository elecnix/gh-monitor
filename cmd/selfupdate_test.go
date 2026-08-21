package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
