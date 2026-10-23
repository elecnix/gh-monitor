//go:build linux

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// listWatchers finds the gh-monitor watcher processes on this machine: every
// process whose command line runs a gh-monitor binary and which is not the
// daemon itself. The scan is what makes "polling outside the daemon" a fact
// rather than a suspicion — nothing gh-monitor logs can see these processes.
func listWatchers(self int) ([]watcherProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scan processes: %w", err)
	}
	var out []watcherProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		argv, err := readArgv(pid)
		if err != nil || !isGhMonitorWatcher(argv) {
			continue
		}
		out = append(out, watcherProcess{
			PID:      pid,
			Age:      procAge(pid),
			Describe: describeArgv(argv),
			Stop:     func() error { return syscall.Kill(pid, syscall.SIGTERM) },
		})
	}
	return out, nil
}

// readArgv returns a process's command line.
func readArgv(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(raw), "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// isGhMonitorWatcher reports whether argv is a gh-monitor process that watches
// (a client), as opposed to the daemon or a sub-daemon, which legitimately
// hold the socket and poll on everyone's behalf.
func isGhMonitorWatcher(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	bin := filepath.Base(argv[0])
	// The runtime copy is named gh-monitor-<key>; the installed binary is
	// gh-monitor. Both are the same program.
	if !strings.HasPrefix(bin, "gh-monitor") && !strings.HasPrefix(bin, "gh_monitor") {
		return false
	}
	// Only the subcommand position names a daemon. Scanning every argument
	// would hide a watcher whose flag value or comment body happens to be the
	// word "daemon" — the invisibility this command exists to remove.
	if len(argv) > 1 && argv[1] == "daemon" {
		return false
	}
	// A daemon spawned with a socket path never watches; sub-daemons are
	// children of the daemon and are named by their config, not by this
	// binary, so anything else running this program is a client.
	return true
}

// procAge is how long a process has been running, from its start time.
func procAge(pid int) time.Duration {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(raw)
	close := strings.LastIndexByte(s, ')')
	if close < 0 {
		return 0
	}
	fields := strings.Fields(s[close+1:])
	// After comm: state(0) ppid(1) ... starttime(19)
	if len(fields) < 20 {
		return 0
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	up, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	upFields := strings.Fields(string(up))
	if len(upFields) == 0 {
		return 0
	}
	upSec, err := strconv.ParseFloat(upFields[0], 64)
	if err != nil {
		return 0
	}
	const hz = 100 // USER_HZ; the kernel reports start times in these ticks
	return time.Duration(upSec*float64(time.Second)) - time.Duration(ticks)*time.Second/hz
}

// describeArgv renders the interesting part of a watcher's command line.
func describeArgv(argv []string) string {
	if len(argv) <= 1 {
		return argv[0]
	}
	rest := argv[1:]
	if len(rest) > 8 {
		rest = rest[:8]
	}
	return strings.Join(rest, " ")
}
