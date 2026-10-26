package ipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestPeerPIDReportsTheOtherEnd pins the kernel-supplied identity: both ends of
// this connection are the test process, so the peer pid is our own. The daemon
// relies on this being the kernel's answer rather than the peer's claim.
func TestPeerPIDReportsTheOtherEnd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "peer.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := l.Accept()
		if aerr == nil {
			accepted <- c
		}
	}()

	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	t.Cleanup(func() { _ = server.Close() })

	if got := PeerPID(client); got != os.Getpid() {
		t.Fatalf("PeerPID from the client side = %d, want %d", got, os.Getpid())
	}
	if got := PeerPID(server); got != os.Getpid() {
		t.Fatalf("PeerPID from the server side = %d, want %d", got, os.Getpid())
	}
}
