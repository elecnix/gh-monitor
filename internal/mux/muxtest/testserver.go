// Package muxtest provides a fake sub-daemon server for tests: it serves the
// backend/remote protocol on a Unix socket exactly as a real sub-daemon
// binary would after the launcher points it at a private socket (issue #88).
// It lives in its own package so both mux's and cmd's test suites can use it
// without an import cycle.
package muxtest

import (
	"context"
	"net"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
)

// StartFakeBackend serves the remote protocol on sockPath, advertising kinds
// and serving src. It is cleaned up with the test.
func StartFakeBackend(t *testing.T, ctx context.Context, sockPath string, kinds []backend.Kind, src backend.Source) {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = remote.Serve(ctx, c, remote.ServerConfig{Name: "fakebroker", Kinds: kinds, Source: src})
			}(conn)
		}
	}()
}
