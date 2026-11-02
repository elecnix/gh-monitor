package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	dir, err := os.MkdirTemp(shortTmpDir(), "notify-*")
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

	// And it must keep saying so, so the next successor in a chain of upgrades
	// inherits the same fact. Asserted through the report a successor would
	// actually read, not by re-reading the same field that was just set — a
	// check that cannot fail is not a check.
	srv := &daemonServer{
		socket:          sock,
		socketActivated: d.socketActivated || d.inheritedActivated,
		info:            d.info,
		started:         time.Now(),
		clients:         newClientRegistry(),
	}
	raw, err := daemonInfoJSON(ctx, srv, srv.started)
	require.NoError(t, err)
	var reported handoff.DaemonInfo
	require.NoError(t, json.Unmarshal(raw, &reported))
	assert.True(t, reported.SocketActivated,
		"the next successor must be told the path is the service manager's, or it unlinks it on shutdown")
}

// TestHandoff_MainPIDIsSentBeforeTheFDPass proves the ordering the design
// depends on, rather than the message's contents. `Adopt` runs BeforeFD before
// it asks for the listening socket, and if that ever inverts the predecessor's
// exit races systemd's SIGCHLD handling — the cgroup kill the notification
// exists to prevent.
//
// Both sides of the race are observed: the notification socket records when
// MAINPID arrives, and the predecessor records when it is asked for its
// descriptor. The assertion is on the order of those two events, so a change
// that sent MAINPID after the fd pass fails here.
func TestHandoff_MainPIDIsSentBeforeTheFDPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	var order []string
	note := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}

	// The notification socket is read as the messages arrive, so MAINPID is
	// stamped in the sequence at the moment it is sent — draining afterwards would
	// only prove it was sent at some point.
	dir, derr := os.MkdirTemp(shortTmpDir(), "ghm-notify-*")
	require.NoError(t, derr)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := filepath.Join(dir, "notify")
	notifyConn, nerr := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	require.NoError(t, nerr)
	t.Cleanup(func() { _ = notifyConn.Close() })
	t.Setenv(sdnotify.SocketEnv, addr)
	t.Setenv("INVOCATION_ID", "inv-order")

	notified := make(chan struct{})
	go func() {
		defer close(notified)
		buf := make([]byte, 4096)
		for {
			n, rerr := notifyConn.Read(buf)
			if rerr != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "MAINPID=") {
				note("mainpid")
				return
			}
		}
	}()

	sock := shortSocket(t, "ghmon-order-*.d")
	l, err := net.Listen("unix", sock)
	require.NoError(t, err)

	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return openPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)
	go func() {
		for {
			conn, aerr := l.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				cfg := remote.ServerConfig{
					Name: DaemonBackendName,
					Source: backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
						return nil, errors.New("unused")
					}),
					HandleOp: func(_ context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
						switch req.Op {
						case handoff.OpInfo:
							raw, _ := json.Marshal(handoff.DaemonInfo{
								PID: os.Getpid() + 1, Socket: sock, Supervised: true,
								InvocationID: "inv-order",
							})
							return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
						case handoff.OpHandoff:
							raw, _ := json.Marshal(h.ExportState())
							return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
						case handoff.OpHandoffFD:
							// The predecessor is being asked for its descriptor: this is
							// the moment MAINPID had to precede.
							note("fd-pass")
							if serr := handoff.SendListener(conn.(*net.UnixConn), l); serr != nil {
								return true, serr
							}
							_ = l.Close()
							return true, nil
						}
						return false, nil
					},
				}
				_ = remote.Serve(ctx, c, cfg)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = l.Close() })

	d := &daemonRuntime{socket: sock, info: daemonInfo{invocationID: "inv-order", supervised: true}}
	listener, _, err := d.listenOrAdopt(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("no MAINPID notification arrived")
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	require.Contains(t, got, "mainpid", "the successor must announce itself")
	require.Contains(t, got, "fd-pass", "and must take the socket")
	assert.Less(t, indexOf(got, "mainpid"), indexOf(got, "fd-pass"),
		"MAINPID must be sent before the listening socket changes hands, or the predecessor's exit races the cgroup teardown")
}

// indexOf returns the position of want in list, or -1.
func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}
