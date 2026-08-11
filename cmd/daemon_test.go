package cmd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closedPR builds a CLOSED PR whose single check suite is COMPLETED/SUCCESS —
// a real terminal CI state (see AGENTS.md). A closed PR makes the daemon
// stream terminate naturally, so single-client tests get a clean EOF.
func closedPR() *monitor.PullRequest {
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "ci"}}
	return &monitor.PullRequest{
		State:   "CLOSED",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid: "aaaaaaa", CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
		}}}},
	}
}

// openPR builds an OPEN PR with a green check suite. An open PR never
// settles, so the daemon stream stays open — used by the two-client sharing
// test to avoid the poller being torn down between subscribers (a flake on
// slow CI runners where the first client's terminal stream ends before the
// second subscribes).
func openPR() *monitor.PullRequest {
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "ci"}}
	return &monitor.PullRequest{
		State:   "OPEN",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid: "aaaaaaa", CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
		}}}},
	}
}

// startTestDaemon wires a hub with a counting fake fetcher to a real Unix
// socket, running serveClient for each connection. prFn supplies each fetch's
// PR payload. It returns the socket path, a fetch-counter, and a cleanup func.
func startTestDaemon(t *testing.T, ctx context.Context, prFn func() *monitor.PullRequest) (socket string, fetches *int64, cleanup func()) {
	t.Helper()
	var calls int64
	fetcher := func(ctx context.Context, id resolver.Identity) (*monitor.PullRequest, error) {
		atomic.AddInt64(&calls, 1)
		return prFn(), nil
	}
	h := hub.New(fetcher, nil, time.Hour, nil)

	// macOS limits Unix socket paths to 104 bytes; t.TempDir() lives under a
	// long /var/folders/... path, so use a short throwaway dir in the system
	// temp instead.
	shortDir, err := os.MkdirTemp("", "ghmon-*.d")
	require.NoError(t, err)
	sock := filepath.Join(shortDir, "d.sock")
	listener, err := ipc.Listen(sock)
	require.NoError(t, err)

	var wg sync.WaitGroup
	serveCtx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				serveClient(serveCtx, h, c)
			}(conn)
		}
	}()

	cleanup = func() {
		cancel()
		_ = listener.Close()
		wg.Wait()
		h.Stop()
		_ = os.RemoveAll(shortDir)
	}
	return sock, &calls, cleanup
}

func daemonSubscribeReq() ipc.Subscribe {
	return ipc.Subscribe{
		Target:   "pr",
		Identity: resolver.Identity{Owner: "o", Repo: "r", Number: 7, Host: "github.com"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 60,
	}
}

// TestDaemon_TwoClientsShareOneFetch is the end-to-end acceptance test for
// issue #34: two `gh monitor` client processes connecting to the daemon must
// both receive the first-poll notification while the daemon makes exactly one
// fetch — proving the shared poller works across a real Unix socket.
//
// It uses an OPEN PR so neither stream ends early: a closed PR would let the
// first client finish and tear down the poller before the second subscribes
// (a flake on slow CI runners, observed as fetches==2).
func TestDaemon_TwoClientsShareOneFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, fetches, cleanup := startTestDaemon(t, ctx, openPR)
	t.Cleanup(cleanup)

	runOpts := runOptsFor() // open PR identity; stream stays open until we cancel

	// Each client streams through a collecting emit and signals when it sees
	// first-poll. The stream stays open (open PR), so we cancel after both have
	// seen first-poll — context.Canceled is the expected return.
	type clientState struct {
		mu        sync.Mutex
		types     []string
		firstPoll chan struct{}
		cctx      context.Context
		cancel    context.CancelFunc
	}
	newClient := func() *clientState {
		cctx, ccancel := context.WithCancel(ctx)
		return &clientState{
			firstPoll: make(chan struct{}),
			cctx:      cctx,
			cancel:    ccancel,
		}
	}
	stages := []*clientState{newClient(), newClient()}

	var wg sync.WaitGroup
	for _, st := range stages {
		st := st
		wg.Add(1)
		go func() {
			defer wg.Done()
			var once sync.Once
			emit := func(n monitor.Notification) {
				st.mu.Lock()
				st.types = append(st.types, n.Type)
				st.mu.Unlock()
				if n.Type == "first-poll" {
					once.Do(func() { close(st.firstPoll) })
				}
			}
			_ = streamFromDaemonAndEmit(st.cctx, sock, runOpts, emit)
			// Returns on cancel with context.Canceled — expected.
		}()
	}

	// Wait for both clients to see first-poll from the shared fetch.
	for i, st := range stages {
		select {
		case <-st.firstPoll:
		case <-time.After(5 * time.Second):
			t.Fatalf("client %d never saw first-poll", i)
		}
	}

	// Both clients received first-poll from a single fetch.
	for i, st := range stages {
		st.mu.Lock()
		saw := contains(st.types, "first-poll")
		st.mu.Unlock()
		assert.True(t, saw, "client %d saw first-poll", i)
	}
	assert.Equal(t, int64(1), atomic.LoadInt64(fetches),
		"two daemon clients must share a single fetch")

	// Cancel both clients so their streams end and the goroutines exit.
	for _, st := range stages {
		st.cancel()
	}
	wg.Wait()
}

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestDaemon_FallsBackWhenSocketAbsent verifies the client path returns an
// os.ErrNotExist-style error when no daemon is running, so runMonitor can
// fall back to in-process polling.
func TestDaemon_FallsBackWhenSocketAbsent(t *testing.T) {
	// Point at a socket that does not exist.
	missing := filepath.Join(t.TempDir(), "nope.sock")
	err := streamFromDaemon(context.Background(), missing, daemonSubscribeReq(), &strings.Builder{})
	assert.True(t, ipc.IsAbsent(err), "expected an absent-socket error, got %v", err)
}

// init guards against the daemon accidentally respecting a real user socket
// when tests run on a developer machine.
func init() {
	_ = os.Unsetenv("GH_MONITOR_SOCK")
}
// runOptsFor is a closed-PR RunOptions whose stream terminates naturally, so
// streamFromDaemonAndEmit returns instead of blocking forever.
func runOptsFor() monitor.RunOptions {
	return monitor.RunOptions{
		Identity: resolver.Identity{Owner: "o", Repo: "r", Number: 7, Host: "github.com"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 60 * time.Second,
		Now:      time.Now,
	}
}

// bindTestServer starts an in-process daemon (hub + accept loop) bound to
// socket, returning the listener for cleanup. It is the test substitute for
// the real spawnDaemon re-exec.
func bindTestServer(t *testing.T, ctx context.Context, h *hub.Hub, socket string) net.Listener {
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
	return l
}

// TestAutoStart_SpawnsDaemonWhenAbsent verifies the autostart wiring: when no
// daemon is listening, streamFromDaemonAndEmit spawns one (via the injectable
// spawnDaemonFn) and streams the first-poll from it.
func TestAutoStart_SpawnsDaemonWhenAbsent(t *testing.T) {
	var fetches int64
	fetcher := func(ctx context.Context, id resolver.Identity) (*monitor.PullRequest, error) {
		atomic.AddInt64(&fetches, 1)
		return closedPR(), nil
	}
	h := hub.New(fetcher, nil, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(h.Stop)

	shortDir, err := os.MkdirTemp("", "ghmon-autostart-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	sock := filepath.Join(shortDir, "d.sock")

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(socket string, interval time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		bindTestServer(t, ctx, h, socket)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	var got []string
	emit := func(n monitor.Notification) { got = append(got, n.Type) }

	err = streamFromDaemonAndEmit(ctx, sock, runOptsFor(), emit)
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(&spawned), "autostart spawned the daemon once")
	assert.Contains(t, got, "first-poll", "client streamed first-poll from the autostarted daemon")
}

// TestAutoStart_SkipsSpawnWhenDaemonRunning verifies autostart does not spawn a
// second daemon when one is already listening.
func TestAutoStart_SkipsSpawnWhenDaemonRunning(t *testing.T) {
	var fetches int64
	fetcher := func(ctx context.Context, id resolver.Identity) (*monitor.PullRequest, error) {
		atomic.AddInt64(&fetches, 1)
		return closedPR(), nil
	}
	h := hub.New(fetcher, nil, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(h.Stop)

	shortDir, err := os.MkdirTemp("", "ghmon-running-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	sock := filepath.Join(shortDir, "d.sock")
	bindTestServer(t, ctx, h, sock) // daemon already up

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(string, time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	var got []string
	emit := func(n monitor.Notification) { got = append(got, n.Type) }

	err = streamFromDaemonAndEmit(ctx, sock, runOptsFor(), emit)
	require.NoError(t, err)
	assert.Equal(t, int64(0), atomic.LoadInt64(&spawned), "must not spawn when a daemon is already running")
	assert.Contains(t, got, "first-poll")
}

// TestAutoStart_DisabledByEnv verifies that GH_MONITOR_AUTOSTART=0 suppresses
// spawning and surfaces an absent-socket error so runMonitor falls back to
// in-process polling.
func TestAutoStart_DisabledByEnv(t *testing.T) {
	t.Setenv("GH_MONITOR_AUTOSTART", "0")

	shortDir, err := os.MkdirTemp("", "ghmon-disabled-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	sock := filepath.Join(shortDir, "d.sock") // never bound

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(string, time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	emit := func(monitor.Notification) {}
	err = streamFromDaemonAndEmit(context.Background(), sock, runOptsFor(), emit)
	assert.True(t, ipc.IsAbsent(err), "expected an absent-socket error, got %v", err)
	assert.Equal(t, int64(0), atomic.LoadInt64(&spawned), "must not spawn when autostart is disabled")
}
