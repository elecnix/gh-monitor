// Package sdnotify speaks the service manager notification protocol
// (sd_notify) so a supervised daemon can tell systemd what it is doing
// (issue #193).
//
// The protocol is one datagram per notification, sent to the Unix socket
// named by $NOTIFY_SOCKET, carrying newline-separated KEY=VALUE assignments.
// It needs no library: this package is the whole client, stdlib only.
//
// Three notifications matter to gh-monitor's daemon:
//
//   - READY=1 once it serves, so a Type=notify unit is started only when
//     clients can actually be answered.
//   - MAINPID=<pid> from an upgrade successor before it takes the listening
//     socket, so systemd follows the handoff instead of treating the
//     predecessor's exit as the service's exit and killing the successor
//     with the rest of the unit's cgroup.
//   - STOPPING=1 on SIGTERM/SIGINT, so a stop is reported as a stop.
//
// Without $NOTIFY_SOCKET every call is a no-op that returns nil: an
// unsupervised daemon, or one supervised by something other than systemd,
// behaves exactly as it did before this package existed.
package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// SocketEnv names the variable the service manager sets to its notification
// socket.
const SocketEnv = "NOTIFY_SOCKET"

// Enabled reports whether a service manager is listening for notifications.
func Enabled() bool { return os.Getenv(SocketEnv) != "" }

// Notify sends one notification carrying the given KEY=VALUE assignments. It
// returns nil without sending when $NOTIFY_SOCKET is unset.
func Notify(assignments ...string) error {
	addr := os.Getenv(SocketEnv)
	if addr == "" {
		return nil
	}
	return send(addr, strings.Join(assignments, "\n"))
}

// Ready reports that the daemon is serving. status is shown by
// `systemctl status`; it may be empty.
func Ready(status string) error {
	if status == "" {
		return Notify("READY=1")
	}
	return Notify("READY=1", "STATUS="+status)
}

// MainPID tells the service manager that pid is now the service's main
// process. An upgrade successor sends it for itself before it takes over the
// listening socket, and sends the predecessor's pid back if the takeover
// fails, so the service's main process is always one that is still serving.
func MainPID(pid int) error {
	return Notify("MAINPID=" + strconv.Itoa(pid))
}

// Stopping reports that the daemon is shutting down at the operator's request.
func Stopping() error { return Notify("STOPPING=1") }

// send writes one datagram to the notification socket. An address starting
// with '@' names a socket in the abstract namespace (Linux), which the
// sockets API spells with a leading NUL byte.
func send(addr, payload string) error {
	name := addr
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("sd_notify: dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(payload)); err != nil {
		return fmt.Errorf("sd_notify: send to %s: %w", addr, err)
	}
	return nil
}
