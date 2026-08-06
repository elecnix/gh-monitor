package cmd

import (
	"bufio"
	"context"
	"encoding/json"
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
// stream terminate naturally, so client tests get a clean EOF instead of
// blocking on an open PR that never settles.
func closedPR() *monitor.PullRequest {
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "ci"}}
	return &monitor.PullRequest{
		State:   "CLOSED",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid: "aaaaaaa", CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
		}}}},
	}
}

// startTestDaemon wires a hub with a counting fake fetcher to a real Unix
// socket, running serveClient for each connection. It returns the socket
// path, a fetch-counter, and a cleanup func.
func startTestDaemon(t *testing.T, ctx context.Context) (socket string, fetches *int64, cleanup func()) {
	t.Helper()
	var calls int64
	fetcher := func(ctx context.Context, id resolver.Identity) (*monitor.PullRequest, error) {
		atomic.AddInt64(&calls, 1)
		return closedPR(), nil
	}
	h := hub.New(fetcher, time.Hour)

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

// firstPollSeen reports whether the NDJSON stream in buf contains a
// first-poll notification.
func firstPollSeen(buf *strings.Builder) bool {
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		var n monitor.Notification
		if err := json.Unmarshal(sc.Bytes(), &n); err == nil && n.Type == "first-poll" {
			return true
		}
	}
	return false
}

// TestDaemon_TwoClientsShareOneFetch is the end-to-end acceptance test for
// issue #34: two `gh monitor` client processes connecting to the daemon must
// both receive the first-poll notification while the daemon makes exactly one
// fetch — proving the shared poller works across a real Unix socket.
func TestDaemon_TwoClientsShareOneFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, fetches, cleanup := startTestDaemon(t, ctx)
	t.Cleanup(cleanup)

	runClient := func() (string, error) {
		var buf strings.Builder
		cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
		defer ccancel()
		err := streamFromDaemon(cctx, sock, daemonSubscribeReq(), &buf)
		return buf.String(), err
	}

	// Two client processes connect and stream from the shared daemon.
	var wg sync.WaitGroup
	bufs := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bufs[i], errs[i] = runClient()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "client %d failed", i)
	}
	// Both clients received the first-poll notification over the socket.
	assert.True(t, firstPollSeen(stringsBuilderFrom(bufs[0])), "client 0 saw first-poll")
	assert.True(t, firstPollSeen(stringsBuilderFrom(bufs[1])), "client 1 saw first-poll")

	// The daemon made exactly one fetch to serve both clients.
	assert.Equal(t, int64(1), atomic.LoadInt64(fetches),
		"two daemon clients must share a single fetch")
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

// stringsBuilderFrom wraps a string as a *strings.Builder for the helper above.
func stringsBuilderFrom(s string) *strings.Builder {
	var b strings.Builder
	b.WriteString(s)
	return &b
}

// init guards against the daemon accidentally respecting a real user socket
// when tests run on a developer machine.
func init() {
	_ = os.Unsetenv("GH_MONITOR_SOCK")
}