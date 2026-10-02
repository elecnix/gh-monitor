package remote

import (
	"bufio"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func socketPath(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// greeting is a daemon-shaped hello: the daemon speaks the protocol and names
// itself, which is all an extension op needs.
const greeting = `{"protocol":1,"name":"daemon","capabilities":["source"]}` + "\n"

// serveOne accepts a single connection and hands it to fn.
func serveOne(t *testing.T, addr string, fn func(net.Conn)) {
	t.Helper()
	l, err := net.Listen("unix", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		fn(c)
	}()
}

// TestConnBoundsASilentPeerAtTheHello covers the handshake: a peer that
// accepts and never greets must not hold the client open, which is what
// HandshakeTimeout is for.
func TestConnBoundsASilentPeerAtTheHello(t *testing.T) {
	addr := socketPath(t, "ghmon-nosay-*")
	serveOne(t, addr, func(c net.Conn) { <-time.After(30 * time.Second) })

	start := time.Now()
	_, err := Dial(t.Context(), "unix", addr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no protocol hello")
	assert.Less(t, time.Since(start), HandshakeTimeout+3*time.Second)
}

// TestConnBoundsASilentPeerAtTheResponse covers the read that follows the
// hello, and is the case the handshake bound does not reach. A peer that
// greets and then stops answering used to park the client in the kernel
// forever, because a context deadline only bounds a read that something
// selects on.
func TestConnBoundsASilentPeerAtTheResponse(t *testing.T) {
	addr := socketPath(t, "ghmon-noreply-*")
	serveOne(t, addr, func(c net.Conn) {
		_, _ = c.Write([]byte(greeting))
		<-time.After(30 * time.Second)
	})

	c, err := Dial(t.Context(), "unix", addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	c.Timeout = 200 * time.Millisecond

	start := time.Now()
	_, err = c.Call(t.Context(), Request{Op: "handoff"})
	require.Error(t, err, "a peer that never answers must not hang the client")
	assert.Less(t, time.Since(start), 10*time.Second)
}

// TestConnRefusesAnotherProtocolVersion guards the upgrade path in the other
// direction: a successor must not hand state to something that will not
// understand it.
func TestConnRefusesAnotherProtocolVersion(t *testing.T) {
	addr := socketPath(t, "ghmon-oldver-*")
	serveOne(t, addr, func(c net.Conn) {
		_, _ = c.Write([]byte(`{"protocol":99,"name":"daemon","capabilities":["source"]}` + "\n"))
	})

	_, err := Dial(t.Context(), "unix", addr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "speaks protocol 99")
}

// TestConnSendsATargetlessRequestVerbatim pins the bytes an extension op
// puts on the wire. The handoff ops cross a version boundary mid-upgrade, so
// this must stay exactly `{"op":"handoff"}` — a zero-valued target object
// would be a wire change made in the one exchange that cannot be retried.
func TestConnSendsATargetlessRequestVerbatim(t *testing.T) {
	addr := socketPath(t, "ghmon-wire-*")
	lines := make(chan string, 1)
	serveOne(t, addr, func(c net.Conn) {
		_, _ = c.Write([]byte(greeting))
		br := bufio.NewReader(c)
		line, _ := br.ReadBytes('\n')
		lines <- string(line)
		_ = WriteFrame(c, Frame{Result: json.RawMessage(`{}`)})
	})

	c, err := Dial(t.Context(), "unix", addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, err = c.Call(t.Context(), Request{Op: "handoff"})
	require.NoError(t, err)
	assert.Equal(t, "{\"op\":\"handoff\"}\n", <-lines)
}

// TestConnSendsATargetedRequest checks the other half of the same framing:
// a request that does name a target still puts it on the wire.
func TestConnSendsATargetedRequest(t *testing.T) {
	addr := socketPath(t, "ghmon-wire2-*")
	lines := make(chan string, 1)
	serveOne(t, addr, func(c net.Conn) {
		_, _ = c.Write([]byte(greeting))
		br := bufio.NewReader(c)
		line, _ := br.ReadBytes('\n')
		lines <- string(line)
		_ = WriteFrame(c, Frame{Result: json.RawMessage(`{}`)})
	})

	c, err := Dial(t.Context(), "unix", addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, err = c.Call(t.Context(), Request{
		Op:     OpRead,
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
	})
	require.NoError(t, err)

	var got struct {
		Op     string         `json:"op"`
		Target backend.Target `json:"target"`
	}
	require.NoError(t, json.Unmarshal([]byte(<-lines), &got))
	assert.Equal(t, OpRead, got.Op)
	assert.Equal(t, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7}, got.Target)
}

// TestConnSendDoesNotWaitForAnAnswer covers the fd pass, where the answer
// arrives as ancillary data on the socket rather than as a frame. Send must
// return as soon as the request is out, leaving the receive to be bounded by
// whatever performs it — Bound, for the handoff's fd pass.
func TestConnSendDoesNotWaitForAnAnswer(t *testing.T) {
	addr := socketPath(t, "ghmon-sendonly-*")
	serveOne(t, addr, func(c net.Conn) {
		_, _ = c.Write([]byte(greeting))
		<-time.After(30 * time.Second)
	})

	c, err := Dial(t.Context(), "unix", addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	start := time.Now()
	// "handoff-fd" by name: the op constant lives in internal/handoff, which
	// is a client of this package. What matters here is only that Send writes
	// the request and returns without reading a reply.
	require.NoError(t, c.Send(Request{Op: "handoff-fd"}))
	assert.Less(t, time.Since(start), 5*time.Second)

	// Bound leaves the connection readable, and the read it arms is the one a
	// caller performs itself; here that is ReadFrame, which re-arms per call.
	require.NoError(t, c.Bound())
	c.Timeout = 200 * time.Millisecond
	_, err = c.ReadFrame()
	require.Error(t, err, "a silent peer must not hang the read")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// TestNewConnClosesTheConnectionItOwns pins the contract NewConn documents:
// taking ownership means closing on every failure path. A caller that hands it
// an already-open socket — the fd-passing handoff path — has no handle left to
// close with, so a leak here is a leak in that caller.
func TestNewConnClosesTheConnectionItOwns(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer func(net.Conn)
	}{
		{
			name: "hello is garbage",
			peer: func(c net.Conn) { _, _ = c.Write([]byte("not json\n")) },
		},
		{
			name: "peer speaks another protocol",
			peer: func(c net.Conn) {
				b, _ := json.Marshal(Hello{Protocol: Protocol + 7, Name: "peer"})
				_, _ = c.Write(append(append(b, '\n'), []byte("{}")...))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mine, theirs := net.Pipe()
			defer func() { _ = theirs.Close() }()

			go tc.peer(theirs)

			if _, err := NewConn(context.Background(), mine); err == nil {
				t.Fatal("NewConn accepted a peer it should reject")
			}
			// The peer learns the socket is gone when its write hits the
			// closed end of the pipe. The deadline bounds this so a leak
			// fails in seconds rather than hanging, and the assertion
			// distinguishes closed from merely slow: a write deadline that
			// expired would also be a non-nil error, and would hide the bug.
			_ = theirs.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, err := theirs.Write([]byte("x"))
			if err == nil {
				t.Fatal("NewConn returned without closing a connection it owns")
			}
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("write failed with %v, want io.ErrClosedPipe (the socket was not closed)", err)
			}
		})
	}
}
