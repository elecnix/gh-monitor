// Package handoff transfers a running daemon's watching state to its
// successor, in memory, over the daemon socket (issue #73).
//
// `gh extension upgrade` rewrites the installed binary in place, which Linux
// refuses with ETXTBSY while any process has it mapped — and the daemon's
// whole job is to stay resident. Rather than making watchers stop or restart,
// an upgraded daemon takes over from the running one:
//
//	successor                          predecessor
//	   │ dial socket, "handoff" op  →      │
//	   │                             ← hub.State (pollers + watcher baselines)
//	   │ dial socket, "handoff-fd" →        │
//	   │                             ← listening socket (SCM_RIGHTS)
//	   │ serves on the adopted socket       │ exits cleanly
//
// The handoff is seamless by construction: no file is written, the successor
// inherits the exact listening socket so clients never see the path unbound,
// and every connected watcher's baseline travels across, so a watcher that
// reconnects resumes diffing where it left off instead of replaying what it
// already reported.
package handoff

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/hub"
)

// Ops the successor sends over the predecessor's socket. They ride the same
// line-delimited JSON framing as backend/remote, on connections that speak
// the same hello-first handshake.
const (
	// OpHandoff asks for the predecessor's transferable state. The response
	// frame's Result carries a hub.State.
	OpHandoff = "handoff"
	// OpHandoffFD asks the predecessor to pass its listening socket. On Unix
	// the fd arrives as ancillary data on this connection; the predecessor
	// shuts down once it has been sent.
	OpHandoffFD = "handoff-fd"
)

// exchangeTimeout bounds each step of the successor's conversation with the
// predecessor. A predecessor that has stopped answering must not stall a
// daemon start; failing the handoff falls back to the ordinary "socket in
// use" error.
const exchangeTimeout = 5 * time.Second

// frame is the subset of remote.Frame the handoff exchange uses.
type frame struct {
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Done   bool            `json:"done,omitempty"`
}

// request sends one op and reads one response frame.
func request(ctx context.Context, socket, op string) (*frame, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("handoff: dial %s: %w", socket, err)
	}
	defer func() { _ = conn.Close() }()

	ctx2, cancel := context.WithDeadline(ctx, time.Now().Add(exchangeTimeout))
	defer cancel()

	br := bufio.NewReader(conn)
	hello, err := readHello(ctx2, conn, br)
	if err != nil {
		return nil, err
	}
	if hello.Protocol != remote.Protocol {
		return nil, fmt.Errorf("handoff: peer speaks protocol %d, this build speaks %d",
			hello.Protocol, remote.Protocol)
	}
	if err := writeJSON(conn, map[string]string{"op": op}); err != nil {
		return nil, fmt.Errorf("handoff: send %s: %w", op, err)
	}
	var f frame
	if err := readJSON(br, &f); err != nil {
		return nil, fmt.Errorf("handoff: read %s response: %w", op, err)
	}
	if f.Error != "" {
		return nil, fmt.Errorf("handoff: %s refused: %s", op, f.Error)
	}
	return &f, nil
}

// Adopt performs the successor side of the handoff against the daemon
// currently holding socket. It returns the adopted listener — on Unix, the
// very socket the predecessor served on — and the predecessor's watching
// state for hub.RestoreState. Any error means the handoff did not happen;
// the caller falls back to its ordinary behaviour.
func Adopt(ctx context.Context, socket string) (net.Listener, hub.State, error) {
	f, err := request(ctx, socket, OpHandoff)
	if err != nil {
		return nil, hub.State{}, err
	}
	if len(f.Result) == 0 {
		return nil, hub.State{}, fmt.Errorf("handoff: empty state response")
	}
	var state hub.State
	if err := json.Unmarshal(f.Result, &state); err != nil {
		return nil, hub.State{}, fmt.Errorf("handoff: decode state: %w", err)
	}

	listener, err := adoptSocket(ctx, socket)
	if err != nil {
		return nil, hub.State{}, err
	}
	return listener, state, nil
}

// adoptSocket acquires the listening socket: on Unix by receiving the
// predecessor's fd over a fresh connection (which also tells the predecessor
// every watcher has been handed off and it may exit); on Windows by
// re-binding the path after the predecessor releases it.
func adoptSocket(ctx context.Context, socket string) (net.Listener, error) {
	uconn, err := dialUnix(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = uconn.Close() }()

	ctx2, cancel := context.WithDeadline(ctx, time.Now().Add(exchangeTimeout))
	defer cancel()
	br := bufio.NewReader(uconn)
	if _, err := readHello(ctx2, uconn, br); err != nil {
		return nil, err
	}
	if err := writeJSON(uconn, map[string]string{"op": OpHandoffFD}); err != nil {
		return nil, fmt.Errorf("handoff: send %s: %w", OpHandoffFD, err)
	}
	return ReceiveListener(uconn, socket)
}

func dialUnix(ctx context.Context, socket string) (*net.UnixConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("handoff: dial %s: %w", socket, err)
	}
	uconn, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("handoff: connection is %T, not a Unix connection", c)
	}
	return uconn, nil
}

// hello mirrors remote.Hello: the handshake the daemon speaks on every
// connection.
type hello struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
}

func readHello(ctx context.Context, conn net.Conn, br *bufio.Reader) (hello, error) {
	type result struct {
		h   hello
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var h hello
		err := readJSON(br, &h)
		ch <- result{h, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return hello{}, fmt.Errorf("handoff: read hello: %w", r.err)
		}
		return r.h, nil
	case <-ctx.Done():
		_ = conn.Close()
		return hello{}, fmt.Errorf("handoff: read hello: %w", ctx.Err())
	}
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func readJSON(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return json.Unmarshal(line, v)
}
