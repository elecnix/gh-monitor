//go:build linux

package handoff

import (
	"net"
	"os"
	"syscall"
)

// PeerPID reports the process id on the other end of a Unix connection, read
// from the kernel's peer credentials. It returns 0 when they are not
// available, which a caller must treat as "unknown", never as "no client": a
// client whose pid cannot be read is still a client.
func PeerPID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	var cred *syscall.Ucred
	if cerr := raw.Control(func(fd uintptr) {
		cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); cerr != nil || cred == nil {
		return 0
	}
	if int(cred.Pid) == os.Getpid() {
		// A daemon talking to itself (a self-probe, a test) is not a client.
		return 0
	}
	return int(cred.Pid)
}
