// Package ipc defines the line-delimited JSON protocol that lets a `gh monitor`
// client attach to a long-lived daemon process (see cmd/daemon.go) and stream
// notifications from a shared poller (issue #34).
//
// Protocol: the client writes one Subscribe request (a single JSON object
// terminated by a newline), then the server streams Notification objects as
// newline-delimited JSON until the client disconnects or the target reaches a
// terminal state.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// Subscribe is the single request a client sends on connect.
type Subscribe struct {
	Target   string             `json:"target"` // "pr" (other targets added later)
	Identity resolver.Identity  `json:"identity"`
	Prefs    prefs.Preferences `json:"prefs"`
	Interval int               `json:"interval"` // seconds; the daemon may clamp
	Timeout  int               `json:"timeout"`  // seconds; 0 = until terminal
}

// DefaultSocketPath returns the daemon socket path. It honours
// $GH_MONITOR_SOCK, then $XDG_RUNTIME_DIR, then a per-user cache dir.
func DefaultSocketPath() string {
	if p := os.Getenv("GH_MONITOR_SOCK"); p != "" {
		return p
	}
	if runDir := os.Getenv("XDG_RUNTIME_DIR"); runDir != "" {
		return runDir + "/gh-monitor.sock"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "gh-monitor", "daemon.sock")
}

// IsAbsent reports whether err indicates no daemon socket is present, so the
// client can fall back to in-process polling.
func IsAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// Listen creates and listens on the Unix socket at path. If a socket file
// already exists, it probes it: a live daemon (Dial succeeds) means another
// process owns it, so Listen returns an error and the caller backs off; a
// stale socket (connection refused) is removed and reused. This makes
// concurrent autostart spawners safe — at most one daemon binds the path.
func Listen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		// A socket file is present. Probe it before touching it.
		if c, derr := net.Dial("unix", path); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("socket %s already in use by a live daemon", path)
		}
		// Stale socket from a crashed daemon — safe to reclaim.
		_ = os.Remove(path)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create socket dir: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	return l, nil
}

// Dial connects to a daemon socket with a short timeout. It returns an error
// satisfying errors.Is(os.ErrNotExist) when no socket is present, so callers
// can fall back to in-process polling.
func Dial(path string) (net.Conn, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err // os.ErrNotExist when no daemon
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.Dial("unix", path)
}

// WaitReady polls the socket until a connection succeeds, the timeout elapses,
// or ctx is cancelled. It is the companion to an autostart spawn: after a
// client forks a daemon it waits for the daemon to bind and accept.
func WaitReady(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if c, err := Dial(path); err == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon socket %s did not come up within %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SendSubscribe writes a single Subscribe request to w.
func SendSubscribe(w io.Writer, req Subscribe) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// ReadSubscribe reads one Subscribe request from r.
func ReadSubscribe(r io.Reader) (Subscribe, error) {
	var req Subscribe
	dec := json.NewDecoder(bufio.NewReader(r))
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

// WriteNotification writes one notification as newline-delimited JSON.
func WriteNotification(w io.Writer, n monitor.Notification) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// ReadSubscribeContext reads a Subscribe request, honouring ctx cancellation.
func ReadSubscribeContext(ctx context.Context, r io.Reader) (Subscribe, error) {
	type result struct {
		req Subscribe
		err error
	}
	ch := make(chan result, 1)
	go func() {
		req, err := ReadSubscribe(r)
		ch <- result{req, err}
	}()
	select {
	case <-ctx.Done():
		return Subscribe{}, ctx.Err()
	case res := <-ch:
		return res.req, res.err
	}
}