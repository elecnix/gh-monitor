package cmd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/elecnix/gh-monitor/internal/sdnotify"
)

// notifySocket stands in for systemd's notification socket and returns the
// messages it received.
func notifySocket(t *testing.T) (addr string, drain func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "notify-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr = filepath.Join(dir, "notify")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return addr, func() []string {
		var got []string
		for {
			buf := make([]byte, 4096)
			_ = l.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, err := l.Read(buf)
			if err != nil {
				return got
			}
			got = append(got, string(buf[:n]))
		}
	}
}

// TestDaemonInfo_ReportsConnectedClients pins OpInfo: a daemon can say what it
// is serving, which is what `gh monitor doctor` compares the machine against.
func TestDaemonInfo_ReportsConnectedClients(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, openPR)
	connectWatcher(t, ctx, sock, "resume-1")

	info, err := handoff.Info(ctx, sock)
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), info.PID, "the in-process test daemon reports this process")
	assert.Equal(t, sock, info.Socket)
	require.Len(t, info.Clients, 1, "the connected watcher must be listed")
	assert.Equal(t, []string{"resume-1"}, info.Clients[0].Resumes)
	assert.True(t, info.Clients[0].Watch)
	require.Len(t, info.Clients[0].Targets, 1)
	assert.Equal(t, 7, info.Clients[0].Targets[0].Number)
}

// TestHandoff_SupervisedSuccessorAnnouncesMainPID is the #193 ordering
// requirement: a successor under a service manager announces itself as the
// new main process *before* it takes the listening socket. Reverse the order
// and the predecessor's exit races systemd's SIGCHLD handling, which is the
// cgroup kill that turned every supervised upgrade into a restart.
func TestHandoff_SupervisedSuccessorAnnouncesMainPID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addr, drain := notifySocket(t)
	t.Setenv(sdnotify.SocketEnv, addr)
	t.Setenv("INVOCATION_ID", "inv-1")

	sock, _ := startTestDaemon(t, ctx, openPR)

	d := &daemonRuntime{
		socket: sock,
		info:   daemonInfo{invocationID: "inv-1", supervised: true},
	}
	l2, state, err := d.listenOrAdopt(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l2.Close() })
	require.NotNil(t, state)

	// The announcement must carry this process's pid, and it must be the only
	// thing sent before the takeover completes.
	msgs := drain()
	require.NotEmpty(t, msgs, "a supervised successor must tell the service manager who it is")
	assert.Contains(t, msgs[0], "MAINPID="+strconv.Itoa(os.Getpid()))
}

// TestHandoff_RefusesTakeoverFromOutsideTheService is the safety half of the
// same fix. A successor that was not started by the unit cannot be its main
// process; taking the socket over would leave a daemon the unit's teardown
// kills, and a predecessor that has already stopped serving. Refusing keeps
// one healthy daemon on the socket.
func TestHandoff_RefusesTakeoverFromOutsideTheService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, openPR)

	d := &daemonRuntime{socket: sock, info: daemonInfo{}} // no INVOCATION_ID: started from a shell
	sup := &daemonRuntime{socket: sock, info: daemonInfo{}}

	// The resident daemon reports a service invocation; this process has none.
	info := handoff.DaemonInfo{InvocationID: "inv-1", PID: os.Getpid()}
	require.Error(t, d.supervisorError(info),
		"a successor outside the service must refuse a supervised daemon")
	assert.NoError(t, sup.supervisorError(handoff.DaemonInfo{}),
		"an unsupervised pair must still hand off as it always did")
	assert.NoError(t, sup.supervisorError(handoff.DaemonInfo{PID: 1}),
		"a predecessor too old to report its invocation must not block the handoff")
}

// TestDaemonReload_StartsSuccessorFromTheDaemon pins OpReload: the daemon,
// not the client, spawns the successor. A client-spawned successor lands in
// the client's cgroup and cannot survive a service manager's teardown.
func TestDaemonReload_StartsSuccessorFromTheDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock := shortSocket(t, "ghmon-reloadop-*.d")
	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return openPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	started := make(chan struct{}, 1)
	var spawnedBy int
	srv := bindTestServer(t, ctx, h, sock)
	srv.respawn = func() (int, error) {
		spawnedBy = os.Getpid()
		started <- struct{}{}
		return 4242, nil
	}

	pid, err := handoff.Reload(ctx, sock)
	require.NoError(t, err)
	assert.Equal(t, 4242, pid)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon did not start the successor itself")
	}
	assert.Equal(t, os.Getpid(), spawnedBy, "the spawn must happen inside the daemon")
}

// TestDaemonReload_NoRespawnReportsWhy: a daemon that cannot spawn says so
// instead of answering success and leaving the operator with a reload that
// never happened.
func TestDaemonReload_NoRespawnReportsWhy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock := shortSocket(t, "ghmon-reloadnone-*.d")
	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return openPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)
	bindTestServer(t, ctx, h, sock) // respawn left nil

	_, err := handoff.Reload(ctx, sock)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not start its successor")
}

// connectWatcher opens a continuous watch against sock, as a client does, and
// returns the updates channel. The stream is torn down when the test ends.
func connectWatcher(t *testing.T, ctx context.Context, sock, resumeID string) <-chan backend.Update {
	t.Helper()
	tr, err := remote.ParseEndpoint("unix:" + sock)
	require.NoError(t, err)
	provider, err := remote.Connect(ctx, tr)
	require.NoError(t, err)
	ch, err := provider.Watch(ctx, daemonTarget(),
		backend.WatchOptions{Interval: time.Hour, ResumeID: resumeID})
	require.NoError(t, err)
	// The poller is created on subscribe, so waiting for the first poll also
	// guarantees the daemon has recorded the client.
	collectUntil(t, ch, backend.EventFirstPoll)
	return ch
}

// TestHandoff_SuccessorDoesNotOwnAnActivatedPath: with socket activation the
// path belongs to the service manager. The successor is handed the listening
// socket, not the activation variables, so it must learn the path is not its
// own from the predecessor — otherwise its shutdown unlinks the socket the
// unit is still serving.
func TestHandoff_SuccessorDoesNotOwnAnActivatedPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock := shortSocket(t, "ghmon-activated-*.d")
	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return openPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)
	l, err := ipc.Listen(sock)
	require.NoError(t, err)
	bindTestServerConfigured(t, ctx, h, l, nil, func(srv *daemonServer) {
		srv.socketActivated = true // what a service manager's socket looks like
	})

	d := &daemonRuntime{socket: sock, info: daemonInfo{}}
	l2, _, err := d.listenOrAdopt(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l2.Close() })
	assert.True(t, d.inheritedActivated,
		"the successor must learn that the path belongs to the service manager, or its shutdown unlinks the unit's socket")

	// And it must keep saying so, so the next successor in a chain of
	// upgrades inherits the same fact.
	assert.True(t, d.socketActivated || d.inheritedActivated,
		"what runDaemon reports for this start must include the inherited fact")
}
