package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/gh"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/elecnix/gh-monitor/internal/subdaemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// init guards against the daemon accidentally respecting a real user socket
// when tests run on a developer machine.
func init() {
	_ = os.Unsetenv("GH_MONITOR_SOCK")
}

// closedPR builds a CLOSED PR whose single check suite is COMPLETED/SUCCESS —
// a real terminal CI state (see AGENTS.md). A closed PR makes the daemon
// stream terminate naturally, so single-client tests get a clean end.
func closedPR() *monitor.PullRequest {
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "ci"}}
	return &monitor.PullRequest{
		State: "CLOSED",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid: "aaaaaaa", CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
		}}}},
	}
}

// openPR builds an OPEN PR with a green check suite. An open PR never settles,
// so the daemon stream stays open — used by the two-client sharing test to
// avoid the poller being torn down between subscribers.
func openPR() *monitor.PullRequest {
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "ci"}}
	return &monitor.PullRequest{
		State: "OPEN",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid: "aaaaaaa", CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
		}}}},
	}
}

func daemonTarget() backend.Target {
	return backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7, Host: "github.com"}
}

// shortSocket returns a socket path under a short directory: macOS caps Unix
// socket paths near 104 bytes, and t.TempDir() is already most of that.
func shortSocket(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// unusedBuiltin is a built-in backend that fails if consulted, so a test
// expecting the daemon to serve cannot pass by silently falling back.
func unusedBuiltin(reg *backend.Registry) {
	reg.RegisterSource(gh.Name, nil, backend.SourceFunc(
		func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
			return nil, errors.New("built-in backend was consulted")
		}))
}

// bindTestServer starts an in-process daemon (hub + accept loop) bound to
// socket. It is the test substitute for the real spawnDaemon re-exec.
func bindTestServer(t *testing.T, ctx context.Context, h *hub.Hub, socket string) {
	t.Helper()
	l, err := ipc.Listen(socket)
	require.NoError(t, err)
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = l.Close()
		wg.Wait()
	})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				serveClient(ctx, h, c)
			}(conn)
		}
	}()
}

// startTestDaemon wires a hub with a counting fake fetcher to a real Unix
// socket. prFn supplies each fetch's PR payload.
func startTestDaemon(t *testing.T, ctx context.Context, prFn func() *monitor.PullRequest) (socket string, fetches *int64) {
	t.Helper()
	var calls int64
	fetcher := func(_ context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&calls, 1)
		return prFn(), nil
	}
	h := hub.New(fetcher, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	sock := shortSocket(t, "ghmon-*.d")
	bindTestServer(t, ctx, h, sock)
	return sock, &calls
}

// collectUntil streams updates until it sees want, returning every event type
// it saw. It fails the test rather than blocking forever.
func collectUntil(t *testing.T, ch <-chan backend.Update, want backend.EventType) []string {
	t.Helper()
	var got []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u, open := <-ch:
			if !open {
				return got
			}
			got = append(got, string(u.Event.Type))
			if u.Event.Type == want {
				return got
			}
		case <-deadline:
			t.Fatalf("never saw %q; got %v", want, got)
			return got
		}
	}
}

// TestDaemon_TwoClientsShareOneFetch is the end-to-end acceptance test for
// issue #34: two `gh monitor` client processes connecting to the daemon must
// both receive the first-poll update while the daemon makes exactly one fetch
// — proving the shared poller works across a real Unix socket.
//
// It uses an OPEN PR so neither stream ends early: a closed PR would let the
// first client finish and tear down the poller before the second subscribes.
func TestDaemon_TwoClientsShareOneFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, fetches := startTestDaemon(t, ctx, openPR)
	t.Setenv("GH_MONITOR_SOCK", sock)

	reg := backend.NewRegistry()
	unusedBuiltin(reg)
	require.NoError(t, attachDaemon(ctx, reg, daemonTarget(), time.Minute))

	source, name, err := reg.SourceFor(daemonTarget())
	require.NoError(t, err)
	require.Equal(t, DaemonBackendName, name, "the daemon must serve a pull request when it is running")

	// Both clients must be subscribed at once: the poller stops when its last
	// consumer leaves, so tearing the first down before the second attaches
	// would legitimately cost a second fetch.
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()

	channels := make([]<-chan backend.Update, 2)
	for i := range channels {
		ch, err := source.Watch(cctx, daemonTarget(), backend.WatchOptions{Interval: time.Minute})
		require.NoError(t, err)
		channels[i] = ch
	}
	for i, ch := range channels {
		assert.Contains(t, collectUntil(t, ch, backend.EventFirstPoll), "first-poll",
			"client %d saw first-poll", i)
	}

	assert.Equal(t, int64(1), atomic.LoadInt64(fetches),
		"two daemon clients must share a single fetch")
}

// TestDaemon_ClientRendersWithItsOwnTemplates is the point of replacing the old
// protocol: the daemon ships events, and the process the operator ran turns
// them into text with that operator's templates.
func TestDaemon_ClientRendersWithItsOwnTemplates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, closedPR)
	t.Setenv("GH_MONITOR_SOCK", sock)

	reg := backend.NewRegistry()
	unusedBuiltin(reg)
	require.NoError(t, attachDaemon(ctx, reg, daemonTarget(), time.Minute))

	source, _, err := reg.SourceFor(daemonTarget())
	require.NoError(t, err)
	ch, err := source.Watch(ctx, daemonTarget(), backend.WatchOptions{Interval: time.Minute})
	require.NoError(t, err)

	custom := prefs.DefaultPreferences()
	custom.Templates["first-poll"] = "WATCHING {owner}/{repo}#{number}"

	var messages []string
	for u := range ch {
		messages = append(messages, monitor.Render(u, custom, time.Minute).Message)
	}

	assert.Contains(t, messages, "WATCHING o/r#7",
		"the client's own template must be what the operator sees")
}

// TestDaemon_NotUsedWhenSocketAbsent verifies that with no daemon listening and
// autostart off, attaching is a hard error — watch mode requires the shared
// poller (issue #76), so a missing daemon must never silently degrade to
// in-process polling.
func TestDaemon_NotUsedWhenSocketAbsent(t *testing.T) {
	t.Setenv("GH_MONITOR_AUTOSTART", "0")
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-absent-*.d")) // never bound

	reg := backend.NewRegistry()
	reg.RegisterSource(gh.Name, nil, backend.SourceFunc(
		func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
			return nil, nil
		}))
	err := attachDaemon(context.Background(), reg, daemonTarget(), time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GH_MONITOR_DAEMON=0",
		"the error must name the escape hatch")
}

// TestDaemon_OptedOutByEnv verifies GH_MONITOR_DAEMON=0 keeps the daemon out of
// the registry even when one is listening — the transition-era escape hatch
// that keeps the in-process loops available.
func TestDaemon_OptedOutByEnv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, openPR)
	t.Setenv("GH_MONITOR_SOCK", sock)
	t.Setenv("GH_MONITOR_DAEMON", "0")

	reg := backend.NewRegistry()
	reg.RegisterSource(gh.Name, nil, backend.SourceFunc(
		func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
			return nil, nil
		}))
	require.NoError(t, attachDaemon(ctx, reg, daemonTarget(), time.Minute))

	_, name, err := reg.SourceFor(daemonTarget())
	require.NoError(t, err)
	assert.Equal(t, gh.Name, name, "GH_MONITOR_DAEMON=0 must keep the daemon unused")
}

// TestDaemon_ServesEveryTargetKind verifies the shared poller multiplexes all
// target kinds (issue #76): a daemon that is running is registered for an
// issue, a ref, and a run just as it is for a pull request.
func TestDaemon_ServesEveryTargetKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, openPR)
	t.Setenv("GH_MONITOR_SOCK", sock)

	reg := backend.NewRegistry()
	unusedBuiltin(reg)
	require.NoError(t, attachDaemon(ctx, reg, daemonTarget(), time.Minute))

	for _, kind := range []backend.Kind{backend.KindIssue, backend.KindRef, backend.KindCommit, backend.KindRun, backend.KindRepo} {
		target := backend.Target{Kind: kind, Owner: "o", Repo: "r", Number: 3, Ref: "main", SHA: "abc", RunID: 9}
		_, name, err := reg.SourceFor(target)
		require.NoError(t, err, kind)
		assert.Equal(t, DaemonBackendName, name, "the daemon must serve %s targets", kind)
	}
}

// TestAutoStart_SpawnsDaemonWhenAbsent verifies the autostart wiring: when no
// daemon is listening, attachDaemon spawns one via the injectable
// spawnDaemonFn and registers it.
func TestAutoStart_SpawnsDaemonWhenAbsent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := hub.New(func(_ context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return closedPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-autostart-*.d"))

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(socket string, _ time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		bindTestServer(t, ctx, h, socket)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	reg := backend.NewRegistry()
	unusedBuiltin(reg)
	require.NoError(t, attachDaemon(ctx, reg, daemonTarget(), time.Minute))

	assert.Equal(t, int64(1), atomic.LoadInt64(&spawned), "autostart spawned the daemon once")
	_, name, err := reg.SourceFor(daemonTarget())
	require.NoError(t, err)
	assert.Equal(t, DaemonBackendName, name)
}

// TestAutoStart_SkipsSpawnWhenDaemonRunning verifies autostart does not spawn a
// second daemon when one is already listening.
func TestAutoStart_SkipsSpawnWhenDaemonRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, closedPR)
	t.Setenv("GH_MONITOR_SOCK", sock)

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(string, time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	reg := backend.NewRegistry()
	unusedBuiltin(reg)
	require.NoError(t, attachDaemon(ctx, reg, daemonTarget(), time.Minute))

	assert.Equal(t, int64(0), atomic.LoadInt64(&spawned), "must not spawn when a daemon is already running")
	_, name, err := reg.SourceFor(daemonTarget())
	require.NoError(t, err)
	assert.Equal(t, DaemonBackendName, name)
}

// TestAutoStart_DisabledByEnv verifies GH_MONITOR_AUTOSTART=0 suppresses
// spawning. Since #76 watch mode requires the shared poller, so attaching is
// a hard error naming the fix — never a silent fall back to in-process
// polling.
func TestAutoStart_DisabledByEnv(t *testing.T) {
	t.Setenv("GH_MONITOR_AUTOSTART", "0")
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-disabled-*.d")) // never bound

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(string, time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	reg := backend.NewRegistry()
	reg.RegisterSource(gh.Name, nil, backend.SourceFunc(
		func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
			return nil, nil
		}))
	require.Error(t, attachDaemon(context.Background(), reg, daemonTarget(), time.Minute))

	assert.Equal(t, int64(0), atomic.LoadInt64(&spawned), "must not spawn when autostart is disabled")
	_, name, err := reg.SourceFor(daemonTarget())
	require.NoError(t, err)
	assert.Equal(t, gh.Name, name)
}

// TestDaemon_SubdaemonMode_SkipsPolling verifies that when a sub-daemon config
// file lists at least one entry, runDaemon enters sub-daemon mode and does NOT
// bind the polling socket — the sub-daemons own it. It overrides the launcher
// to a fake whose child exits as a not-found start, so the supervisor gives up
// immediately and runDaemon returns without binding anything.
func TestDaemon_SubdaemonMode_SkipsPolling(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemons.conf")
	require.NoError(t, os.WriteFile(cfgPath, []byte("broker-subscriber /no/such/binary daemon\n"), 0o644))
	t.Setenv("GH_MONITOR_SUBDAEMONS", cfgPath)
	// A socket path that, if polling mode ran, would be bound by ipc.Listen.
	// Use a never-created path so that if runDaemon incorrectly binds it, the
	// test can detect the leftover file.
	sock := filepath.Join(dir, "polling.sock")
	t.Setenv("GH_MONITOR_SOCK", sock)

	orig := subdaemonLauncherFn
	t.Cleanup(func() { subdaemonLauncherFn = orig })
	subdaemonLauncherFn = func(entries []subdaemon.Entry, out io.Writer) *subdaemon.Launcher {
		// Instant, no-real-process launcher: every entry's binary is missing,
		// so superviseOne logs once and returns without spawning anything.
		return &subdaemon.Launcher{
			Entries:       entries,
			Out:           out,
			MinBackoff:    time.Millisecond,
			MaxBackoff:    time.Millisecond,
			MaxRapidFails: 1,
			Sleep:         func(time.Duration) {},
		}
	}

	cmd := newDaemonCommand()
	cmd.SetArgs([]string{"--socket", sock, "--interval", "10"})
	err := cmd.Execute()
	require.NoError(t, err)

	// The polling socket must NOT have been bound in sub-daemon mode.
	_, statErr := os.Stat(sock)
	assert.True(t, os.IsNotExist(statErr),
		"sub-daemon mode must not bind the polling socket; stat err=%v", statErr)
}

// TestDaemon_NoConfigFallsBackToPolling verifies that with no sub-daemon config
// file, the daemon's sub-daemon loader is a no-op and the polling path is
// unchanged. This is a guard against a regression that reads the config even
// when the file is absent and accidentally enters sub-daemon mode.
func TestDaemon_NoConfigFallsBackToPolling(t *testing.T) {
	// Point the config path at a path that does not exist.
	t.Setenv("GH_MONITOR_SUBDAEMONS", filepath.Join(t.TempDir(), "absent.conf"))

	loaded, ok, err := subdaemon.Load(subdaemon.DefaultConfigPath())
	require.NoError(t, err)
	assert.False(t, ok, "absent config must read as not-configured, not an error")
	assert.Empty(t, loaded)
}
