//go:build !linux

package ipc

import "net"

// PeerPID returns the process id at the other end of a Unix connection. Only
// Linux exposes SO_PEERCRED through the syscall package without a cgo call
// (the BSDs and macOS spell it differently), so elsewhere the answer is
// "unknown" — 0 — and callers that need a peer identity do nothing.
func PeerPID(net.Conn) int { return 0 }
