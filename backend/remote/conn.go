package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// ExchangeTimeout bounds each write and each read that follows the hello on
// one connection.
//
// The hello has its own, shorter bound (HandshakeTimeout) because a peer that
// never greets is a known failure. Everything after it is a request/response
// pair with a peer that has already proved it speaks the protocol, so it gets
// a longer window — but it must still be a window.
//
// The bound belongs on the socket, not only on the context. A context
// deadline interrupts a blocking read only if something selects on it, and
// the deferred Close that would unblock the read runs precisely when it
// returns. A context deadline alone therefore bounds nothing here: the read
// parks in the kernel and the timeout expires unobserved beside it.
const ExchangeTimeout = 5 * time.Second

// Conn is the client half of the protocol for a single connection: dial,
// read and check the hello, then send one request and read one response
// frame.
//
// It exists because the codec — readHello, writeJSON, readJSON — is
// unexported. Without it, every in-tree client that speaks the protocol has
// to re-declare the framing, and a re-declared codec is a codec that does
// not inherit the package's bounds. internal/handoff is the case that
// mattered: its copy of the framing carried a context deadline that bounded
// nothing, so the upgrade handoff against a stalled daemon blocked forever.
type Conn struct {
	// Timeout bounds each operation that follows the hello. Zero means
	// ExchangeTimeout. It is consulted per operation, so a caller may set it
	// after dialing.
	Timeout time.Duration

	conn  net.Conn
	br    *bufio.Reader
	hello Hello
}

// wireRequest is Request as it goes on the wire. Target and Options are
// pointers so an exchange that has no target — an extension op such as the
// upgrade handoff's, which names no PR — does not put an empty target object
// on the wire, where the protocol never defined one.
type wireRequest struct {
	Op      string                `json:"op"`
	Target  *backend.Target       `json:"target,omitempty"`
	Options *backend.WatchOptions `json:"options,omitempty"`
	Payload json.RawMessage       `json:"payload,omitempty"`
}

// Dial opens a connection to addr and completes the handshake, refusing a
// peer that announces a different protocol version.
func Dial(ctx context.Context, network, addr string) (*Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	c, err := NewConn(ctx, conn)
	if err != nil {
		// NewConn owns the connection from here and closes it on every
		// failure path, so there is nothing left to do here.
		return nil, err
	}
	return c, nil
}

// NewConn completes the handshake on an already-open connection, taking
// ownership of it: on failure the connection is closed.
func NewConn(ctx context.Context, conn net.Conn) (*Conn, error) {
	c := &Conn{conn: conn, br: bufio.NewReader(conn)}
	// The socket gets a deadline too. readHello closes the connection when
	// HandshakeTimeout fires, but the deadline is what bounds the read if
	// that goroutine is ever not the one that loses the race. It is the
	// longer ExchangeTimeout, not HandshakeTimeout, so that the closing
	// goroutine always wins the race rather than sometimes losing it to a
	// read that fails first and reports a timeout nobody was waiting for.
	_ = conn.SetDeadline(time.Now().Add(ExchangeTimeout))
	// From this point the connection is ours, and every way out closes it.
	// A caller that hands NewConn a connection it opened itself would
	// otherwise leak it on a protocol mismatch or a failed hello, since it
	// has no handle left to close with.
	fail := func(err error) (*Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	hello, err := readHello(ctx, conn, c.br)
	if err != nil {
		return fail(err)
	}
	if hello.Protocol != Protocol {
		return fail(fmt.Errorf("peer speaks protocol %d, this build speaks %d", hello.Protocol, Protocol))
	}
	c.hello = hello
	// The handshake deadline was armed for the hello alone, and it is an
	// absolute instant, so leaving it on the socket would silently bound
	// everything that follows. Send and ReadFrame each arm their own bound
	// per operation, which is why a lapsed one cannot fail them — but
	// UnixConn hands out the raw connection for a caller that arms nothing,
	// and that read would inherit an instant chosen for a different purpose
	// and possibly already in the past. Clearing it costs no guarantee: a
	// Conn's socket carries a deadline exactly when a Conn method asked for
	// one.
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

// Hello is the opening frame the peer sent.
func (c *Conn) Hello() Hello { return c.hello }

// UnixConn returns the underlying connection as a *net.UnixConn, for the
// exchanges whose answer is the socket itself rather than a frame. The second
// result is false on any other kind of connection.
func (c *Conn) UnixConn() (*net.UnixConn, bool) {
	uconn, ok := c.conn.(*net.UnixConn)
	return uconn, ok
}

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// deadline returns the bound to apply to one operation.
func (c *Conn) deadline() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return ExchangeTimeout
}

// Call sends one request and reads the response frame. A frame carrying an
// error is returned as an error, so callers handle only the success shape.
func (c *Conn) Call(ctx context.Context, req Request) (Frame, error) {
	if err := c.Send(req); err != nil {
		return Frame{}, err
	}
	frame, err := c.ReadFrame()
	if err != nil {
		return Frame{}, err
	}
	if frame.Error != "" {
		return Frame{}, errors.New(frame.Error)
	}
	return frame, nil
}

// Send writes one request without waiting for a response, for the exchanges
// that answer out of band.
func (c *Conn) Send(req Request) error {
	wire := wireRequest{Op: req.Op, Payload: req.Payload}
	if req.Target != (backend.Target{}) {
		wire.Target = &req.Target
	}
	// WatchOptions holds a slice, so zero-ness is a deep comparison.
	if !reflect.DeepEqual(req.Options, backend.WatchOptions{}) {
		wire.Options = &req.Options
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.deadline())); err != nil {
		return fmt.Errorf("arm write deadline: %w", err)
	}
	if err := writeJSON(c.conn, wire); err != nil {
		return fmt.Errorf("send %s: %w", req.Op, err)
	}
	return nil
}

// ReadFrame reads one response frame, giving up after the exchange bound
// rather than waiting on a peer that has stopped answering.
func (c *Conn) ReadFrame() (Frame, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(c.deadline())); err != nil {
		return Frame{}, fmt.Errorf("arm read deadline: %w", err)
	}
	var frame Frame
	if err := readJSON(c.br, &frame); err != nil {
		return Frame{}, fmt.Errorf("read response: %w", err)
	}
	return frame, nil
}

// Bound arms the connection deadline for a read that happens outside the
// framing — the upgrade handoff receives its answer as ancillary data on a
// Unix socket, not as a frame. Without it that read has no bound at all.
func (c *Conn) Bound() error {
	if err := c.conn.SetDeadline(time.Now().Add(c.deadline())); err != nil {
		return fmt.Errorf("arm deadline: %w", err)
	}
	return nil
}
