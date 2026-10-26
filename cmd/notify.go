// Service-manager notifications (issue #193).
//
// A supervised daemon used to lose its upgrade handoff to its supervisor: the
// detached successor stayed inside the unit's cgroup, the predecessor's exit
// was read as the *service's* exit, and systemd answered by killing the rest
// of the cgroup — the successor that had just adopted the listening socket and
// the carried watching state — then restarting from the installed binary. The
// upgrade landed, but as a restart: baselines and snapshots were thrown away
// and every watcher re-bootstrapped.
//
// Naming the successor as the unit's main process before the predecessor goes
// away is what removes that race. A unit then needs
//
//	[Service]
//	Type=notify
//	NotifyAccess=all
//
// and no GH_MONITOR_REEXEC opt-out, so the installed binary is never busy and
// `gh extension upgrade` lands while the daemon runs.
package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/sdnotify"
)

// Notification seams. Package variables, like maybeReexecFn, so tests can
// assert what a supervised daemon tells its manager with no manager present.
var (
	sdEnabledFn    = sdnotify.Enabled
	sdSupervisedFn = sdnotify.Supervised
	sdMainPIDFn    = sdnotify.MainPID
	sdReadyFn      = sdnotify.Ready
	sdStoppingFn   = sdnotify.Stopping
)

// announceSuccessor tells the service manager that this process — not the
// predecessor it is about to hand off from — is the daemon to supervise.
//
// The announcement must come *before* the handoff dial, never after it. The
// predecessor exits as soon as it has passed its listening socket over, and a
// manager that processes that exit first tears down the whole cgroup, taking
// the successor with it. Dialling first would race those two events, so this
// deliberately probes the socket and speaks before the handoff starts.
//
// A daemon that finds no live predecessor says nothing: it is an ordinary
// start, and the manager already treats it as the main process — after a
// runtime-copy re-exec that is still the PID the manager forked.
//
// Neither does a successor started from an operator's shell: `gh monitor
// reload` spawns one, outside the unit's cgroup, and naming it would take the
// unit away from the daemon the manager supervises. Supervised is the test for
// that — not the socket's peer identity, because a Unix socket reports the pid
// of the process that *created* it, and a handoff inherits the listening
// socket: from the second handoff onward the kernel still names the original
// daemon (see internal/sdnotify/supervised_linux.go).
func announceSuccessor(socket string, stderr io.Writer) {
	if !sdEnabledFn() || !sdSupervisedFn() {
		return
	}
	c, err := ipc.Dial(socket)
	if err != nil {
		return // no live predecessor: an ordinary start
	}
	_ = c.Close()
	if err := sdMainPIDFn(os.Getpid()); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"gh-monitor daemon: could not name the successor to the service manager (%v); it may restart the unit instead of following the handoff\n", err)
	}
}

// announceReady reports that the daemon is serving: the socket is bound or
// adopted, and the hub — with any carried watching state — is restored. A
// Type=notify unit waits for exactly this before it calls the service started.
func announceReady(stderr io.Writer) {
	announce(sdReadyFn, "readiness", stderr)
}

// announceStopping reports a clean shutdown in progress, so a manager that
// asked for notifications records the transition instead of only the timeout
// that ends in a SIGKILL.
func announceStopping(stderr io.Writer) {
	announce(sdStoppingFn, "shutdown", stderr)
}

// announce sends one notification for a daemon that a manager asked about,
// logging — never failing — when the manager's socket cannot be written.
// Polling must never depend on a supervisor, so a notification is best-effort
// by construction.
func announce(send func() error, what string, stderr io.Writer) {
	if !sdEnabledFn() {
		return
	}
	if err := send(); err != nil {
		_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: could not report %s to the service manager (%v)\n", what, err)
	}
}
