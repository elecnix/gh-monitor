//go:build unix

// Sending the listening socket from the predecessor daemon to its successor
// over the handoff connection (SCM_RIGHTS), so the successor serves on the
// very socket the predecessor held — no gap, no re-bind, no window in which
// a client can find no daemon (issue #73).

package handoff

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// SendListener passes l's listening socket file descriptor to the peer on the
// other end of conn. The predecessor stops unlinking the socket path first —
// the successor now owns it and is responsible for removing it on its own
// shutdown.
func SendListener(conn *net.UnixConn, l net.Listener) error {
	ul, ok := l.(*net.UnixListener)
	if !ok {
		return fmt.Errorf("handoff: listener is %T, not a Unix listener", l)
	}
	ul.SetUnlinkOnClose(false)
	lf, err := ul.File()
	if err != nil {
		return fmt.Errorf("handoff: duplicate listening socket: %w", err)
	}
	defer func() { _ = lf.Close() }()

	cf, err := conn.File()
	if err != nil {
		return fmt.Errorf("handoff: duplicate connection: %w", err)
	}
	defer func() { _ = cf.Close() }()

	if err := syscall.Sendmsg(int(cf.Fd()), []byte{'!'}, syscall.UnixRights(int(lf.Fd())), nil, 0); err != nil {
		return fmt.Errorf("handoff: send listening socket: %w", err)
	}
	return nil
}

// ReceiveListener receives the listening socket sent by SendListener and
// wraps it back into a net.Listener. The socket path keeps pointing at the
// same inode; the successor is responsible for removing it on its own
// shutdown.
func ReceiveListener(conn *net.UnixConn, socket string) (net.Listener, error) {
	buf := make([]byte, 1)
	oob := make([]byte, 64) // room for one SCM_RIGHTS message
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("handoff: receive listening socket: %w", err)
	}
	if n != 1 || oobn == 0 {
		return nil, fmt.Errorf("handoff: no listening socket arrived (%d bytes, %d oob)", n, oobn)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("handoff: parse control message: %w", err)
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("handoff: expected 1 control message, got %d", len(msgs))
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		return nil, fmt.Errorf("handoff: parse socket rights: %w", err)
	}
	if len(fds) != 1 {
		return nil, fmt.Errorf("handoff: expected 1 file descriptor, got %d", len(fds))
	}
	f := os.NewFile(uintptr(fds[0]), socket)
	l, err := net.FileListener(f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("handoff: adopt listening socket: %w", err)
	}
	return l, nil
}
