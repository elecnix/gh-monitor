package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawWatchServer is a hand-rolled server for stream-break tests: it speaks
// the hello/watch framing directly, so a test can drop a connection mid-watch
// the way a daemon restart does.
type rawWatchServer struct {
	ln        net.Listener
	requests  chan Request
	accepted  int32
	resumable bool
}

func startRawWatchServer(t *testing.T, resumable bool) *rawWatchServer {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	s := &rawWatchServer{ln: ln, requests: make(chan Request, 4), resumable: resumable}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

// serve answers the first connection with one update and an abrupt close —
// the shape of a daemon dying mid-watch — and every later connection with a
// second update and a clean end-of-stream.
func (s *rawWatchServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	hello := Hello{
		Protocol:     Protocol,
		Name:         "daemon",
		Capabilities: []backend.Capability{backend.CapSource},
		Kinds:        []backend.Kind{backend.KindPR},
		Resumable:    s.resumable,
	}
	if err := writeJSON(conn, hello); err != nil {
		return
	}
	var req Request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		return // a probe: Connect dials, reads the hello, and hangs up
	}
	n := atomic.AddInt32(&s.accepted, 1)
	s.requests <- req

	typ := "update-two"
	if n == 1 {
		typ = "update-one"
	}
	update := backend.Update{Target: prTarget(), Event: backend.Event{Type: backend.EventType(typ)}}
	if err := writeJSON(conn, Frame{Update: &update}); err != nil {
		return
	}
	if n == 1 {
		return // abrupt close: no end-of-stream, the transport just dies
	}
	_ = writeJSON(conn, Frame{Done: true})
}

// collectTypes drains ch, labelling degraded frames by their message so the
// test can tell the break notice from the reconnect notice.
func collectTypes(t *testing.T, ch <-chan backend.Update) []string {
	t.Helper()
	var out []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			switch {
			case u.Event.Type == backend.EventDegraded && u.Event.Notice != "":
				out = append(out, "notice:"+u.Event.Notice)
			case u.Event.Type == backend.EventDegraded:
				out = append(out, "degraded:"+u.Event.DegradedMessage)
			default:
				out = append(out, string(u.Event.Type))
			}
		case <-deadline:
			t.Fatalf("timed out collecting events, got %v", out)
		}
	}
}

// TestWatchReconnectsAcrossABrokenStream is the watcher's side of the upgrade
// handoff (issue #73): a resumable backend's stream breaks, the watcher says
// so loudly once, re-establishes the stream with the same ResumeID, and keeps
// delivering — instead of ending the watch.
func TestWatchReconnectsAcrossABrokenStream(t *testing.T) {
	srv := startRawWatchServer(t, true)
	tr, err := ParseEndpoint("unix:" + srv.ln.Addr().String())
	require.NoError(t, err)
	p, err := Connect(context.Background(), tr)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, err := p.Watch(ctx, prTarget(), backend.WatchOptions{ResumeID: "watcher-1"})
	require.NoError(t, err)

	got := collectTypes(t, ch)

	assert.Equal(t, []string{
		"update-one",
		"degraded:daemon: connection lost (stream ended without an end-of-stream frame); reconnecting",
		"notice:✅ reconnected to daemon; resuming the watch from where it left off",
		"update-two",
	}, got, "the watch must ride across the broken stream")

	// Both connections must carry the same ResumeID, so the server can find
	// the watcher's carried baseline.
	close(srv.requests)
	var seen []string
	for req := range srv.requests {
		seen = append(seen, req.Options.ResumeID)
	}
	assert.Equal(t, []string{"watcher-1", "watcher-1"}, seen,
		"both connections must carry the same ResumeID")
}

// TestWatchEndsWhenBackendIsNotResumable pins today's behaviour for backends
// that did not announce resumability: a broken stream degrades and ends the
// watch rather than retrying a backend that may simply be gone.
func TestWatchEndsWhenBackendIsNotResumable(t *testing.T) {
	srv := startRawWatchServer(t, false)
	tr, err := ParseEndpoint("unix:" + srv.ln.Addr().String())
	require.NoError(t, err)
	p, err := Connect(context.Background(), tr)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, err := p.Watch(ctx, prTarget(), backend.WatchOptions{})
	require.NoError(t, err)

	got := collectTypes(t, ch)
	assert.Equal(t, []string{
		"update-one",
		"degraded:daemon: stream ended without an end-of-stream frame",
	}, got, "a non-resumable backend's broken stream ends the watch after degrading")
}
