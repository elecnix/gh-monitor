package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/elecnix/gh-monitor/internal/subdaemon"
	"github.com/stretchr/testify/require"
)

// fakeChild is a subdaemon.Process whose Wait blocks until Signal is called,
// mimicking a well-behaved sub-daemon that exits cleanly on SIGTERM.
type fakeChild struct {
	stop chan struct{}
	once sync.Once
}

func newFakeChild() *fakeChild { return &fakeChild{stop: make(chan struct{})} }

func (f *fakeChild) Wait() (time.Duration, error) {
	<-f.stop
	return 0, nil
}

func (f *fakeChild) Signal(sig os.Signal) error {
	f.once.Do(func() { close(f.stop) })
	return nil
}

// runSubdaemonForTest runs runSubdaemonMode with a launcher that spawns fresh
// fake children, counting every generation. It returns a reader for the
// generation count and a cancel for the daemon's root context.
func runSubdaemonForTest(t *testing.T, entries []subdaemon.Entry, socket string, interval time.Duration) (func() int, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	// The upgrade watcher stats the installed binary on this cadence; the
	// default 15s would outlast every test deadline here.
	origInterval := upgradeCheckInterval
	upgradeCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { upgradeCheckInterval = origInterval })
	var mu sync.Mutex
	generations := 0
	subdaemonLauncherFn = func(entries []subdaemon.Entry, out io.Writer) *subdaemon.Launcher {
		mu.Lock()
		generations++
		mu.Unlock()
		return &subdaemon.Launcher{
			Entries: entries,
			Out:     io.Discard,
			Spawn: func(ctx context.Context, entry subdaemon.Entry) (subdaemon.Process, error) {
				return newFakeChild(), nil
			},
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cmd := newDaemonCommand()
		_ = runSubdaemonMode(ctx, cmd, entries, socket, interval)
	}()
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return generations
	}, cancel, &wg
}

// TestSubdaemonUpgrade_HandsOffOnBinaryChange verifies the sub-daemon-mode
// upgrade end to end: when the installed binary changes, the launcher stops
// its children (releasing their sockets), spawns the upgraded binary as the
// successor, and — once the successor proves alive through the grace window —
// this process exits. Service converges on the new generation.
func TestSubdaemonUpgrade_HandsOffOnBinaryChange(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
	t.Setenv(reexec.OptOutEnv, "0") // keep MaybeReexec out of the test path
	t.Setenv(reexec.InstalledBinEnv, installed)

	var spawns int32
	origSpawn := spawnUpgradedDaemonFn
	t.Cleanup(func() { spawnUpgradedDaemonFn = origSpawn })
	spawnUpgradedDaemonFn = func(installed, socket string, interval time.Duration) (*os.Process, error) {
		atomic.AddInt32(&spawns, 1)
		return nil, nil // successor "started"; liveness is stubbed below
	}
	origAlive := subdaemonSuccessorAliveFn
	t.Cleanup(func() { subdaemonSuccessorAliveFn = origAlive })
	subdaemonSuccessorAliveFn = func(*os.Process) bool { return true }

	entries := []subdaemon.Entry{{Name: "fake", Cmd: []string{"fake", "child"}}}
	generations, cancel, wg := runSubdaemonForTest(t, entries, filepath.Join(dir, "d.sock"), time.Second)
	t.Cleanup(cancel)

	// Let the watcher record its baseline before the upgrade lands.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(installed, []byte("v2-with-a-longer-body"), 0o755))

	deadline := time.After(5 * time.Second)
	for atomic.LoadInt32(&spawns) == 0 {
		select {
		case <-deadline:
			t.Fatal("the upgraded daemon was never spawned on binary change")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The children of the pre-upgrade generation must have been stopped so
	// they release their sockets before the successor launches its own.
	deadline = time.After(5 * time.Second)
	for generations() != 1 {
		select {
		case <-deadline:
			t.Fatalf("expected exactly 1 generation after handoff, got %d", generations())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
}

// TestSubdaemonUpgrade_SuccessorFailsRelaunchesChildren verifies convergence
// when the successor cannot even be spawned: the children are relaunched on
// the old binary and the watcher keeps going.
func TestSubdaemonUpgrade_SuccessorFailsRelaunchesChildren(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
	t.Setenv(reexec.OptOutEnv, "0")
	t.Setenv(reexec.InstalledBinEnv, installed)

	origSpawn := spawnUpgradedDaemonFn
	t.Cleanup(func() { spawnUpgradedDaemonFn = origSpawn })
	spawnUpgradedDaemonFn = func(installed, socket string, interval time.Duration) (*os.Process, error) {
		return nil, fmt.Errorf("spawn failed")
	}

	entries := []subdaemon.Entry{{Name: "fake", Cmd: []string{"fake", "child"}}}
	generations, cancel, _ := runSubdaemonForTest(t, entries, filepath.Join(dir, "d.sock"), time.Second)
	t.Cleanup(cancel)

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(installed, []byte("v2-bigger"), 0o755))

	// The relaunch must bring up a second generation on the old binary.
	deadline := time.After(5 * time.Second)
	for generations() < 2 {
		select {
		case <-deadline:
			t.Fatalf("children were never relaunched after a failed spawn; generations=%d", generations())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
}

// TestSubdaemonUpgrade_SuccessorDiesInGraceRelaunchesChildren verifies the
// other failure side: a successor that spawns but dies within the takeover
// grace window must not become an outage — the old binary's children come
// back.
func TestSubdaemonUpgrade_SuccessorDiesInGraceRelaunchesChildren(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
	t.Setenv(reexec.OptOutEnv, "0")
	t.Setenv(reexec.InstalledBinEnv, installed)

	origSpawn := spawnUpgradedDaemonFn
	t.Cleanup(func() { spawnUpgradedDaemonFn = origSpawn })
	spawnUpgradedDaemonFn = func(installed, socket string, interval time.Duration) (*os.Process, error) {
		return nil, nil
	}
	origAlive := subdaemonSuccessorAliveFn
	t.Cleanup(func() { subdaemonSuccessorAliveFn = origAlive })
	subdaemonSuccessorAliveFn = func(*os.Process) bool { return false } // successor dies immediately

	origGrace := upgradeTakeoverTimeout
	upgradeTakeoverTimeout = 50 * time.Millisecond
	t.Cleanup(func() { upgradeTakeoverTimeout = origGrace })

	entries := []subdaemon.Entry{{Name: "fake", Cmd: []string{"fake", "child"}}}
	generations, cancel, _ := runSubdaemonForTest(t, entries, filepath.Join(dir, "d.sock"), time.Second)
	t.Cleanup(cancel)

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(installed, []byte("v2-bigger"), 0o755))

	deadline := time.After(5 * time.Second)
	for generations() < 2 {
		select {
		case <-deadline:
			t.Fatalf("children were never relaunched after the successor died; generations=%d", generations())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
}

// TestSubdaemon_SelfUpdateArmed verifies the release checker runs in
// sub-daemon mode too when preferences enable it — the setting must not be
// inert just because the polling hub is off (#84).
func TestSubdaemon_SelfUpdateArmed(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
	t.Setenv(reexec.OptOutEnv, "0")
	t.Setenv(reexec.InstalledBinEnv, installed)

	writeSelfUpdatePrefs(t, "5ms")

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

	entries := []subdaemon.Entry{{Name: "fake", Cmd: []string{"fake", "child"}}}
	_, cancel, wg := runSubdaemonForTest(t, entries, filepath.Join(dir, "d.sock"), time.Second)
	t.Cleanup(cancel)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := checks
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("selfUpdate in preferences never armed the release checker in sub-daemon mode")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
}
