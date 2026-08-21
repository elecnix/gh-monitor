//go:build windows

package handoff

import (
	"fmt"
	"net"
	"time"
)

// SendListener is not implemented on Windows: SCM_RIGHTS does not exist
// there. The predecessor skips it and shuts down; the successor re-binds.
func SendListener(conn *net.UnixConn, l net.Listener) error {
	return nil
}

// ReceiveListener re-binds the socket path instead of adopting a file
// descriptor: the predecessor has closed its listener (and released the
// path) by the time the successor calls this, so a short retry covers the
// exit race. The state transfer is still in memory; only the listening
// socket is re-created, and only on Windows.
func ReceiveListener(conn *net.UnixConn, socket string) (net.Listener, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := net.Listen("unix", socket)
		if err == nil {
			return l, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("handoff: re-bind %s: %w", socket, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
