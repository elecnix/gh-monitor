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
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/hub"
)

// Ops the successor sends over the predecessor's socket. They ride the same
// line-delimited JSON framing as the rest of the protocol, and they are sent
// through remote.Conn, so the handshake and the exchange bounds apply here
// exactly as they do to any other client.
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
//
// It is applied to the socket by remote.Conn, which is the only thing that
// actually bounds a blocking read. It is a var so tests can shorten it.
var exchangeTimeout = remote.ExchangeTimeout

// request sends one op and reads one response frame.
func request(ctx context.Context, socket, op string) (remote.Frame, error) {
	c, err := remote.Dial(ctx, "unix", socket)
	if err != nil {
		return remote.Frame{}, fmt.Errorf("handoff: dial %s: %w", socket, err)
	}
	defer func() { _ = c.Close() }()
	c.Timeout = exchangeTimeout

	frame, err := c.Call(ctx, remote.Request{Op: op})
	if err != nil {
		return remote.Frame{}, fmt.Errorf("handoff: %s: %w", op, err)
	}
	return frame, nil
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
	c, err := remote.Dial(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("handoff: dial %s: %w", socket, err)
	}
	defer func() { _ = c.Close() }()
	c.Timeout = exchangeTimeout

	uconn, ok := c.UnixConn()
	if !ok {
		return nil, fmt.Errorf("handoff: handoff fd pass requires a Unix socket")
	}
	if err := c.Send(remote.Request{Op: OpHandoffFD}); err != nil {
		return nil, fmt.Errorf("handoff: %s: %w", OpHandoffFD, err)
	}
	// The answer to this op is not a frame but the listening socket itself,
	// passed as ancillary data, so it needs its deadline armed by hand. It
	// also needs it most: the predecessor has already been told to hand the
	// socket over and will not exit until it has, so a successor that waits
	// forever here leaves two daemons on one socket, no error, and no
	// fallback — the documented "socket in use" failure can never run.
	if err := c.Bound(); err != nil {
		return nil, fmt.Errorf("handoff: %w", err)
	}
	return ReceiveListener(uconn, socket)
}
