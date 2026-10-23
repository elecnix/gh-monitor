//go:build !linux

package handoff

import "net"

// PeerPID reports the process id on the other end of a Unix connection. Peer
// credentials are a Linux facility here, so this reports 0, which callers
// treat as "unknown" — the daemon still records its clients, it just cannot
// name their pids.
func PeerPID(net.Conn) int { return 0 }
