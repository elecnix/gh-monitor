// Package sdnotify speaks the service-manager notification protocol
// (sd_notify), so a supervised daemon can report that it is serving and, across
// an upgrade handoff, which process has become the unit's new main one
// (issue #193).
//
// The protocol is one datagram of newline-separated KEY=VALUE assignments sent
// to the socket named by $NOTIFY_SOCKET. Nothing is answered: a manager that
// never reads the socket is indistinguishable from one that agreed, so the
// only failure worth reporting is a send that could not be attempted at all.
//
// Everything here is inert when $NOTIFY_SOCKET is unset, which is the state of
// every install that no service manager started.
package sdnotify

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// NotifySocketEnv names the environment variable a service manager uses to
// tell the process where to send its notifications.
const NotifySocketEnv = "NOTIFY_SOCKET"

// Enabled reports whether a service manager asked this process for
// notifications. It is the single condition every caller gates on, so an
// unsupervised install never pays for a socket call.
func Enabled() bool { return os.Getenv(NotifySocketEnv) != "" }

// Ready reports that the daemon is serving. A Type=notify unit waits for
// exactly this before it considers the service started; without it, systemd
// kills the daemon when TimeoutStartSec elapses.
func Ready() error { return Send("READY=1") }

// MainPID reports that pid is now the process the manager should supervise.
//
// This is what keeps an upgrade handoff seamless under systemd: the successor
// has adopted the listening socket and the watching state, so the unit must
// follow it rather than conclude, from its predecessor's exit, that the
// service ended. The successor is in the unit's cgroup and stays there, so a
// manager that is told early enough never tears that cgroup down.
func MainPID(pid int) error { return Send("MAINPID=" + strconv.Itoa(pid)) }

// Stopping reports that a clean shutdown has begun, so `systemctl stop`
// records the transition instead of only the SIGKILL that ends the timeout.
func Stopping() error { return Send("STOPPING=1") }

// Send posts one state assignment to the service manager. No manager, no
// datagram, no error: being unsupervised is a normal state, not a failure.
func Send(state string) error {
	addr := os.Getenv(NotifySocketEnv)
	if addr == "" {
		return nil
	}
	if err := post(socketName(addr), []byte(state)); err != nil {
		return fmt.Errorf("sd_notify %q to %s: %w", state, addr, err)
	}
	return nil
}

// socketName resolves $NOTIFY_SOCKET to the address the kernel expects.
//
// A leading '@' is this protocol's spelling of an abstract-namespace socket,
// which the kernel represents as a leading NUL byte. Getting the translation
// wrong does not fail loudly — the datagram goes nowhere and the manager waits
// forever — so it is explicit here and covered by a test against a real
// abstract listener.
func socketName(addr string) string {
	if strings.HasPrefix(addr, "@") {
		return "\x00" + addr[1:]
	}
	return addr
}

// WithoutNotifyEnv returns env with the notification variable removed.
//
// A child process must not speak for the service: a sub-daemon that sent
// READY=1 would mark the unit started before the daemon that owns the socket
// is serving, and one that sent MAINPID would hand the unit to a process that
// does not own it (issue #193). The daemon's own successor is deliberately not
// filtered — it is the process the manager must follow.
func WithoutNotifyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, NotifySocketEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
