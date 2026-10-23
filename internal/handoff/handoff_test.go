package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortPath keeps unix socket paths under macOS's ~104-byte limit.
func shortPath(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// TestSendReceiveListener verifies the fd pass end to end: a listener sent
// over a Unix connection comes back as a working listener on the same path.
func TestSendReceiveListener(t *testing.T) {
	sockPath := shortPath(t, "ghmon-fd-*.d")
	target, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })

	// The control connection the fd travels over.
	ctrl, err := net.Listen("unix", shortPath(t, "ghmon-ctrl-*.d"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctrl.Close() })

	serverDone := make(chan error, 1)
	go func() {
		c, err := ctrl.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- SendListener(c.(*net.UnixConn), target)
	}()

	client, err := net.Dial("unix", ctrl.Addr().String())
	require.NoError(t, err)
	received, err := ReceiveListener(client.(*net.UnixConn), sockPath)
	require.NoError(t, err)
	require.NoError(t, <-serverDone)
	t.Cleanup(func() { _ = received.Close() })

	// The adopted listener serves on the same path...
	assert.Equal(t, sockPath, received.Addr().String())
	_, statErr := os.Stat(sockPath)
	assert.NoError(t, statErr, "the socket path must survive the handoff")

	// ...and actually accepts.
	go func() {
		c, err := net.Dial("unix", sockPath)
		require.NoError(t, err)
		_ = c.Close()
	}()
	accepted, err := received.Accept()
	require.NoError(t, err)
	_ = accepted.Close()
}

// startPredecessor runs an in-process stand-in for the old daemon: it serves
// the backend protocol on socket, answers the handoff ops from hub and
// listener, and shuts down when the fd has been passed — the same wiring as
// cmd.daemonServer.
func startPredecessor(t *testing.T, ctx context.Context, socket string, h *hub.Hub, stateful bool) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				cfg := remote.ServerConfig{
					Name: "daemon",
					Source: backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
						return nil, errors.New("not used in this test")
					}),
				}
				if stateful {
					cfg.HandleOp = func(ctx context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
						switch req.Op {
						case OpInfo:
							raw, err := json.Marshal(DaemonInfo{PID: os.Getpid(), Socket: socket})
							if err != nil {
								return true, remote.WriteFrame(conn, remote.Frame{Error: err.Error()})
							}
							return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
						case OpHandoff:
							raw, err := json.Marshal(h.ExportState())
							if err != nil {
								return true, remote.WriteFrame(conn, remote.Frame{Error: err.Error()})
							}
							return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
						case OpHandoffFD:
							if err := SendListener(conn.(*net.UnixConn), l); err != nil {
								return true, err
							}
							_ = l.Close() // the successor owns the socket now
							return true, nil
						}
						return false, nil
					}
				}
				_ = remote.Serve(ctx, c, cfg)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
}

func handoffHub(t *testing.T) *hub.Hub {
	t.Helper()
	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return &monitor.PullRequest{State: "OPEN"}, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, cancelSub := h.SubscribePR(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7}, backend.WatchOptions{})
	t.Cleanup(cancelSub)
	// Wait until the poller has fetched at least once, so the exported state
	// carries a snapshot.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.ExportState(); len(st.Pollers) == 1 && st.Pollers[0].Latest != nil {
			return h
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hub never produced its first snapshot")
	return nil
}

// TestAdoptTransfersStateAndSocket is the successor side of the whole
// handoff: state arrives, the listening socket is adopted on the same path,
// and the predecessor stops serving.
func TestAdoptTransfersStateAndSocket(t *testing.T) {
	socket := shortPath(t, "ghmon-adopt-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := handoffHub(t)
	startPredecessor(t, ctx, socket, h, true)

	listener, state, err := Adopt(ctx, socket, Options{})
	require.NoError(t, err)
	require.NotNil(t, listener)
	t.Cleanup(func() { _ = listener.Close() })

	require.Len(t, state.Pollers, 1, "the watched PR must travel")
	assert.Equal(t, 7, state.Pollers[0].Identity.Number)

	// The adopted listener serves the same path and accepts connections.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the adopted listener never accepted a connection")
	}
}

// stalledPredecessor runs an in-process stand-in for a daemon that is
// mid-upgrade and has stopped making progress: it greets every connection,
// then blocks forever inside the named op without answering it. Because it
// blocks rather than returns, the connection stays open, so a client waiting
// on it waits for real — the condition the exchange bound exists for.
func stalledPredecessor(t *testing.T, ctx context.Context, socket string, h *hub.Hub, stallOn string) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	release := make(chan struct{})
	t.Cleanup(func() { close(release); _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				cfg := remote.ServerConfig{
					Name: "daemon",
					Source: backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
						return nil, errors.New("not used in this test")
					}),
					HandleOp: func(ctx context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
						if req.Op == stallOn {
							<-release
							return true, nil
						}
						if req.Op == OpHandoff {
							raw, err := json.Marshal(h.ExportState())
							if err != nil {
								return true, remote.WriteFrame(conn, remote.Frame{Error: err.Error()})
							}
							return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
						}
						return false, nil
					},
				}
				_ = remote.Serve(ctx, c, cfg)
			}(conn)
		}
	}()
}

// boundAdopt runs Adopt against socket with the exchange bound shortened, and
// reports how long it took. Production uses remote.ExchangeTimeout; the test
// shortens it so a bound that is not actually enforced shows up as a test
// timeout rather than as a stall.
//
// Shortening a package-level var is only safe while the package's tests run
// one at a time, so this is the one place in package handoff that writes
// shared state and it is written for the duration of a single test. A test
// that calls t.Parallel() would overlap with this write and race against
// Adopt's read of the same variable; if one is ever added, this has to become
// a parameter instead. Nothing in the package parallelises today — that is
// what makes the write safe, not the absence of a race detector.
func boundAdopt(t *testing.T, socket string) (time.Duration, error) {
	t.Helper()
	prev := exchangeTimeout
	exchangeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { exchangeTimeout = prev })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	listener, _, err := Adopt(ctx, socket, Options{})
	if listener != nil {
		_ = listener.Close()
	}
	return time.Since(start), err
}

// TestAdoptBoundedWhenPredecessorNeverGreets covers the hello itself, the
// one step that happens inside remote.Dial — before the exchange bound is
// ever set on the connection, because the bound is applied by the Conn the
// dial returns. A predecessor that accepts the connection and never writes a
// hello must not park the successor, and the caller's context is not enough:
// context.Background() carries no deadline, and a context deadline does not
// interrupt a blocking socket read anyway.
//
// The hello read is bounded by remote.HandshakeTimeout inside the shared
// codec, so this asserts the bound is reached rather than assuming where it
// comes from — the point of the test is that Adopt returns, not that a
// particular package owns the timeout.
func TestAdoptBoundedWhenPredecessorNeverGreets(t *testing.T) {
	socket := shortPath(t, "ghmon-nogreet-*.d")

	// A predecessor that accepts and says nothing at all.
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Never write a hello; hold the connection open until the
			// test tears the listener down.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	// context.Background() carries no deadline, which is the finding's
	// precondition.
	start := time.Now()
	listener, _, err := Adopt(context.Background(), socket, Options{})
	elapsed := time.Since(start)
	if listener != nil {
		_ = listener.Close()
	}

	require.Error(t, err, "a predecessor that never greets must not hang the successor")
	assert.Contains(t, err.Error(), "no protocol hello")
	assert.Less(t, elapsed, 30*time.Second, "the hello read must give up on its own")
}

// TestAdoptBoundedWhenPredecessorNeverAnswers covers the wait for the
// predecessor's state response. The exchange bound is set on the context, but
// a context deadline does not interrupt a blocking socket read — so without a
// deadline on the socket itself, this call never returns.
func TestAdoptBoundedWhenPredecessorNeverAnswers(t *testing.T) {
	socket := shortPath(t, "ghmon-silent-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stalledPredecessor(t, ctx, socket, handoffHub(t), OpHandoff)

	elapsed, err := boundAdopt(t, socket)
	require.Error(t, err, "a predecessor that never answers must not hang the successor")
	assert.Less(t, elapsed, 10*time.Second, "the exchange must give up on its own")
}

// TestAdoptBoundedWhenPredecessorNeverPassesTheSocket covers the fd pass,
// the one step whose answer is not a frame at all. It is also the one step
// that wedges both daemons: the predecessor has already been told to hand the
// socket over and will not exit until it has, so a successor blocked in the
// receive leaves two daemons on one socket and no fallback.
func TestAdoptBoundedWhenPredecessorNeverPassesTheSocket(t *testing.T) {
	socket := shortPath(t, "ghmon-nofd-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stalledPredecessor(t, ctx, socket, handoffHub(t), OpHandoffFD)

	elapsed, err := boundAdopt(t, socket)
	require.Error(t, err, "a predecessor that never passes the socket must not hang the successor")
	assert.Less(t, elapsed, 10*time.Second, "the fd receive must give up on its own")
}

// TestAdoptRefusedByOldDaemon verifies the successor falls back cleanly when
// the running daemon predates the handoff ops: Adopt reports an error, and
// the caller surfaces the ordinary "socket in use" failure.
func TestAdoptRefusedByOldDaemon(t *testing.T) {
	socket := shortPath(t, "ghmon-old-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := handoffHub(t)
	startPredecessor(t, ctx, socket, h, false)

	_, _, err := Adopt(ctx, socket, Options{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupported,
		"an old daemon is a fallback, not a fault: the caller must be able to tell them apart")
}

// TestAdoptAbortsAnnouncementOnAnyLaterFailure pins where the undo belongs.
// AbortFD was wired to the fd pass alone, so a failed state read left a
// supervised successor announced as the service's main process while the
// predecessor kept serving — a MAINPID naming a process that never took over.
func TestAdoptAbortsAnnouncementOnAnyLaterFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		// serve answers the info request, then fails the state request the
		// way a predecessor that stops answering does.
		failState bool
	}{
		{name: "state read fails", failState: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := shortPath(t, "ghmon-abort-*.d")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			l, err := net.Listen("unix", socket)
			require.NoError(t, err)
			t.Cleanup(func() { _ = l.Close() })
			go func() {
				for {
					conn, aerr := l.Accept()
					if aerr != nil {
						return
					}
					go func(c net.Conn) {
						defer func() { _ = c.Close() }()
						cfg := remote.ServerConfig{
							Name: "daemon",
							Source: backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
								return nil, errors.New("unused")
							}),
							HandleOp: func(_ context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
								if req.Op != OpInfo {
									return false, nil
								}
								raw, _ := json.Marshal(DaemonInfo{PID: 4242, Socket: socket})
								return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
							},
						}
						_ = remote.Serve(ctx, c, cfg)
					}(conn)
				}
			}()

			var abortedWith DaemonInfo
			aborts := 0
			_, _, err = Adopt(ctx, socket, Options{
				BeforeFD: func(DaemonInfo) error { return nil },
				AbortFD: func(pre DaemonInfo) {
					aborts++
					abortedWith = pre
				},
			})
			require.Error(t, err, "the handoff cannot complete without the state")
			assert.Equal(t, 1, aborts,
				"an announcement must be undone when the takeover fails after it was made")
			assert.Equal(t, 4242, abortedWith.PID,
				"the undo must name the predecessor, so the service points back at the process still serving")
		})
	}
}

// TestAdoptDoesNotAbortWhenBeforeFDRefused: a refusal happens before anything
// was announced, so there is nothing to undo — and announcing a correction for
// an announcement nobody made would itself be a lie.
func TestAdoptDoesNotAbortWhenBeforeFDRefused(t *testing.T) {
	socket := shortPath(t, "ghmon-refuse-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := handoffHub(t)
	startPredecessor(t, ctx, socket, h, true)

	aborts := 0
	_, _, err := Adopt(ctx, socket, Options{
		BeforeFD: func(DaemonInfo) error { return errors.New("refused") },
		AbortFD:  func(DaemonInfo) { aborts++ },
	})
	require.Error(t, err)
	assert.Zero(t, aborts)
}

// TestAdoptDoesNotAnnounceForAMissingDaemon pins what a failed Info means.
// Only "the daemon predates daemon-info" is a reason to carry on undescribed;
// anything else — no socket, nothing listening, a refused connection — means
// there is no predecessor to take over from, and announcing one would name a
// successor for a daemon that does not exist.
func TestAdoptDoesNotAnnounceForAMissingDaemon(t *testing.T) {
	socket := shortPath(t, "ghmon-missing-*.d") // never bound
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	announced, aborted := 0, 0
	_, _, err := Adopt(ctx, socket, Options{
		BeforeFD: func(DaemonInfo) error { announced++; return nil },
		AbortFD:  func(DaemonInfo) { aborted++ },
	})
	require.Error(t, err, "there is no daemon on that socket to hand off from")
	assert.Zero(t, announced, "a missing daemon must not be announced at all")
	assert.Zero(t, aborted, "and nothing was announced, so there is nothing to undo")
}

// TestAdoptAnnouncesForADaemonThatCannotDescribeItself keeps the other side:
// an older daemon answers "unsupported op", which is a real predecessor and
// must not block the takeover.
func TestAdoptAnnouncesForADaemonThatCannotDescribeItself(t *testing.T) {
	socket := shortPath(t, "ghmon-old-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, aerr := l.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				// A daemon from before OpInfo: it answers nothing it does not
				// know, and never hands its socket over either.
				_ = remote.Serve(ctx, c, remote.ServerConfig{
					Name: "daemon",
					Source: backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
						return nil, errors.New("unused")
					}),
				})
			}(conn)
		}
	}()

	announced := 0
	var got DaemonInfo
	_, _, err = Adopt(ctx, socket, Options{
		BeforeFD: func(pre DaemonInfo) error { announced++; got = pre; return nil },
	})
	assert.Equal(t, 1, announced,
		"a predecessor too old to describe itself is still a predecessor")
	assert.Equal(t, socket, got.Socket)
	assert.Zero(t, got.PID, "and it is described honestly, with no invented pid")
	assert.Error(t, err, "the state read still fails, so the handoff does not complete")
}
