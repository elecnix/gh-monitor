package ipc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// OwnerPath is the file beside a daemon socket that names the process serving
// it.
//
// A dial that is refused cannot tell a dead daemon from a live one that is not
// accepting: macOS refuses a connect when the accept queue is full, and a
// crashed daemon's leftover socket file refuses it too. Listen used to read
// every refusal as "stale" and unlink the path, which left the live daemon
// running on an orphaned socket while its successor bound a new one. The
// record is the second witness: a refusal is stale only when the process that
// wrote it is gone.
func OwnerPath(socket string) string { return socket + ".pid" }

// ClaimOwner records this process as the one serving socket. Listen does it on
// bind; a successor that adopted the listening socket from a predecessor calls
// it, because Listen never ran for it.
func ClaimOwner(socket string) error {
	if err := os.WriteFile(OwnerPath(socket), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("record the daemon owning %s: %w", socket, err)
	}
	return nil
}

// ReleaseOwner removes the record when it still names this process. After a
// handoff it names the successor, and removing that would hide a live daemon.
func ReleaseOwner(socket string) {
	if pid, ok := recordedOwner(socket); ok && pid == os.Getpid() {
		_ = os.Remove(OwnerPath(socket))
	}
}

// recordedOwner reads the pid from the record.
func recordedOwner(socket string) (int, bool) {
	raw, err := os.ReadFile(OwnerPath(socket))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// ownerAlive reports whether a process other than this one is recorded as
// serving socket and still exists. No record means a daemon from before the
// record existed, or one that never wrote it, and is read as not alive: that
// is the behaviour Listen always had.
func ownerAlive(socket string) bool {
	pid, ok := recordedOwner(socket)
	return ok && pid != os.Getpid() && processExists(pid)
}
