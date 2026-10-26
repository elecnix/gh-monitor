//go:build linux

package sdnotify

import (
	"os"
	"strings"
)

// cgroupFile is where the kernel reports this process's cgroup membership.
var cgroupFile = "/proc/self/cgroup"

// Supervised reports whether this process runs inside a systemd *service*
// cgroup — that is, whether a unit owns it as a unit owns its main process and
// its children.
//
// It is the precondition a manager itself applies to a MAINPID announcement: a
// manager accepts a change of main process only for a process it supervises,
// and it supervises a cgroup, not a pid. That makes this the honest test for
// "may this process speak for the unit", and it is the only test that survives
// a handoff — see supervisedByCgroup for why the obvious alternative does not.
//
// An unreadable file means "not supervised": a daemon should stay silent about
// a unit it cannot prove it belongs to.
func Supervised() bool {
	raw, err := os.ReadFile(cgroupFile)
	if err != nil {
		return false
	}
	return supervisedByCgroup(string(raw))
}

// supervisedByCgroup reports whether any entry of /proc/self/cgroup names a
// service unit as its leaf.
//
// It reads the file's contents rather than a live socket credential on purpose.
// SO_PEERCRED identifies the process that called listen() on a Unix socket, and
// a handoff passes that listening socket to a successor as an inherited
// descriptor — so from the second handoff onward the kernel still reports the
// *original* creator's pid, not the daemon actually accepting connections. A
// guard built on that pid passes once and then fails on every later handoff,
// which under systemd means the unit is restarted rather than followed.
//
// The file is one line per controller hierarchy on cgroup v1
// ("12:pids:/user.slice/…/gh-monitor-daemon.service") and a single "0::" line on
// v2; both end the path with the leaf unit. A scope — session-3.scope, the leaf
// of a process an operator started from their desktop session — is deliberately
// not a service: a daemon started from a shell must not take a unit away from
// the daemon that unit supervises.
func supervisedByCgroup(contents string) bool {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// controller-list:path — the path is everything after the second colon.
		_, path, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		_, path, ok = strings.Cut(path, ":")
		if !ok {
			continue
		}
		leaf := path[strings.LastIndex(path, "/")+1:]
		if strings.HasSuffix(leaf, ".service") {
			return true
		}
	}
	return false
}
