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

// TestWatch_RevivesAServerThatIsNotComingBack is the client half of "a watcher
// never polls on its own and never sits silent": when reconnecting stops
// making progress, the watch asks for the server to be restarted, and says so.
// A watcher that only retried would be indistinguishable from a quiet target
// (issue #193).
func TestWatch_RevivesAServerThatIsNotComingBack(t *testing.T) {
	srv := startRawWatchServer(t, true)
	tr, err := ParseEndpoint("unix:" + srv.ln.Addr().String())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p, err := Connect(ctx, tr)
	require.NoError(t, err)

	revived := make(chan struct{}, 4)
	p.SetRevive(func(context.Context) error {
		select {
		case revived <- struct{}{}:
		default:
		}
		return nil
	})

	// The server accepts one connection, streams, then drops it. Every later
	// attempt is refused by closing the listener, so reconnecting cannot make
	// progress.
	ch, err := p.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7, Host: "github.com"},
		backend.WatchOptions{Interval: time.Hour, ResumeID: "r1"})
	require.NoError(t, err)
	_ = srv.ln.Close()

	select {
	case <-revived:
	case <-time.After(10 * time.Second):
		t.Fatal("a watch that cannot reconnect must ask for the server to be restarted")
	}
	// Drain so the goroutine is not left blocked on the channel.
	cancel()
	for range ch { //nolint:revive // drain until closed
	}
}

// TestShouldRevive covers the restart cadence: the first attempt comes after
// reviveAfterAttempts failed reconnects, and it repeats, because a restart
// that succeeds is not proof the server stayed up. Firing once per streak left
// a watcher reconnecting to a socket nobody held for the rest of the watch —
// the outcome revive exists to prevent.
func TestShouldRevive(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     bool
	}{
		{1, false}, {4, false},
		{5, true}, // first attempt of a streak
		{6, false}, {14, false},
		{15, true}, // a second attempt, ten failures later
		{25, true},
		{26, false},
	} {
		assert.Equal(t, tc.want, shouldRevive(tc.attempts), "attempt %d", tc.attempts)
	}
}
