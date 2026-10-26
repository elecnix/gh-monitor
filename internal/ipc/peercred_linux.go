//go:build linux

package ipc

import (
	"net"
	"syscall"
)

// PeerPID returns the process id at the other end of a Unix connection, or 0
// when it cannot be determined.
//
// It reads SO_PEERCRED, the kernel's own answer to "who am I really talking
// to": the credentials are attached by the kernel at connect time, so a peer
// cannot claim to be someone else. The daemon uses it to tell its own
// upgrade successor — a child it spawned — from a daemon started by an
// unrelated client, which must not be announced to a service manager.
func PeerPID(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		var cred *syscall.Ucred
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if credErr == nil && cred != nil {
			pid = int(cred.Pid)
		}
	}); err != nil || credErr != nil {
		return 0
	}
	return pid
}
