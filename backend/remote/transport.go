package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Transport opens one connection to a backend. Each Watch or Read gets its
// own connection, so a stalled or failed stream never affects another.
type Transport interface {
	Open(ctx context.Context) (io.ReadWriteCloser, error)
	// String renders the endpoint for diagnostics.
	String() string
}

// TransportFunc adapts a function to the Transport interface, and names it.
func TransportFunc(name string, open func(ctx context.Context) (io.ReadWriteCloser, error)) Transport {
	return &funcTransport{name: name, open: open}
}

type funcTransport struct {
	name string
	open func(ctx context.Context) (io.ReadWriteCloser, error)
}

func (t *funcTransport) Open(ctx context.Context) (io.ReadWriteCloser, error) { return t.open(ctx) }
func (t *funcTransport) String() string                                       { return t.name }

// ParseEndpoint builds a Transport from an endpoint string:
//
//	unix:/path/to/socket     connect to a Unix socket
//	tcp:host:port            connect over TCP
//	exec:command --args      run a command and talk over its stdin/stdout
//
// An unknown or empty scheme is rejected rather than guessed at, so a
// mistyped endpoint fails at startup instead of silently monitoring nothing.
func ParseEndpoint(s string) (Transport, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty backend endpoint")
	}
	scheme, addr, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("backend endpoint %q has no scheme (expected unix:, tcp:, or exec:)", s)
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("backend endpoint %q has no address", s)
	}
	switch scheme {
	case "unix", "tcp":
		return &netTransport{network: scheme, addr: addr}, nil
	case "exec":
		fields := strings.Fields(addr)
		return &execTransport{argv: fields, raw: addr}, nil
	default:
		return nil, fmt.Errorf("unsupported backend endpoint scheme %q (expected unix, tcp, or exec)", scheme)
	}
}

// netTransport dials a Unix socket or TCP address.
type netTransport struct {
	network string
	addr    string
}

func (t *netTransport) Open(ctx context.Context) (io.ReadWriteCloser, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, t.network, t.addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", t, err)
	}
	return conn, nil
}

func (t *netTransport) String() string { return t.network + ":" + t.addr }

// execTransport runs a command and speaks the protocol over its stdio. The
// command's stderr is passed through to ours so a backend's diagnostics stay
// visible rather than disappearing into the pipe.
type execTransport struct {
	argv []string
	raw  string
}

func (t *execTransport) Open(ctx context.Context) (io.ReadWriteCloser, error) {
	if len(t.argv) == 0 {
		return nil, fmt.Errorf("exec endpoint has no command")
	}
	cmd := exec.CommandContext(ctx, t.argv[0], t.argv[1:]...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", t.argv[0], err)
	}
	return &procConn{cmd: cmd, in: stdin, out: stdout}, nil
}

func (t *execTransport) String() string { return "exec:" + t.raw }

// procConn presents a child process's stdio as a connection.
type procConn struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
}

func (c *procConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *procConn) Write(p []byte) (int, error) { return c.in.Write(p) }

// Close shuts the child down: closing stdin asks it to finish, and the
// process is killed if it does not.
func (c *procConn) Close() error {
	_ = c.in.Close()
	_ = c.out.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}
