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
	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (*monitor.PullRequest, error) {
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

	listener, state, err := Adopt(ctx, socket)
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

// TestAdoptRefusedByOldDaemon verifies the successor falls back cleanly when
// the running daemon predates the handoff ops: Adopt reports an error, and
// the caller surfaces the ordinary "socket in use" failure.
func TestAdoptRefusedByOldDaemon(t *testing.T) {
	socket := shortPath(t, "ghmon-old-*.d")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := handoffHub(t)
	startPredecessor(t, ctx, socket, h, false)

	_, _, err := Adopt(ctx, socket)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported op")
}
