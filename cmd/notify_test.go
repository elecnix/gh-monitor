package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/mux"
	"github.com/elecnix/gh-monitor/internal/subdaemon"
)

// stubNotifications replaces the sd_notify seams for one test, enabled, and
// records what the daemon said. Notification state is process-global, so these
// tests never run in parallel — the same convention maybeReexecFn follows.
type notifyRecorder struct {
	mu      sync.Mutex
	order   []string
	mainPID []int
	stderr  bytes.Buffer
}

func stubNotifications(t *testing.T, enabled bool) *notifyRecorder {
	t.Helper()
	rec := &notifyRecorder{}
	origEnabled, origSupervised, origMainPID, origReady, origStopping :=
		sdEnabledFn, sdSupervisedFn, sdMainPIDFn, sdReadyFn, sdStoppingFn
	t.Cleanup(func() {
		sdEnabledFn, sdSupervisedFn, sdMainPIDFn, sdReadyFn, sdStoppingFn =
			origEnabled, origSupervised, origMainPID, origReady, origStopping
	})
	sdEnabledFn = func() bool { return enabled }
	sdSupervisedFn = func() bool { return enabled }
	sdMainPIDFn = func(pid int) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.mainPID = append(rec.mainPID, pid)
		rec.order = append(rec.order, "MAINPID")
		return nil
	}
	sdReadyFn = func() error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.order = append(rec.order, "READY")
		return nil
	}
	sdStoppingFn = func() error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.order = append(rec.order, "STOPPING")
		return nil
	}
	return rec
}

func (r *notifyRecorder) snapshot() (order []string, pids []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...), append([]int(nil), r.mainPID...)
}

// TestSuccessorNamesItselfBeforeTheHandoff is the ordering rule issue #193
// turns on: the successor must claim the unit *before* it asks the predecessor
// for the socket, because the predecessor exits as soon as the socket has
// moved and a manager that sees that exit first kills the whole cgroup — the
// successor included.
func TestSuccessorNamesItselfBeforeTheHandoff(t *testing.T) {
	rec := stubNotifications(t, true)
	sock := shortSocket(t, "ghmon-sd-*.sock")

	// A live predecessor: something is listening on the daemon socket.
	predecessor, err := ipc.Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = predecessor.Close() })

	announceSuccessor(sock, &rec.stderr)

	_, pids := rec.snapshot()
	require.Equal(t, []int{os.Getpid()}, pids,
		"a successor must name its own pid as the unit's main process")
	assert.Empty(t, rec.stderr.String(), "a successful announcement is silent")
}

// TestAReloadSuccessorDoesNotClaimTheUnit covers the other spawner: `gh monitor
// reload` starts a successor from the operator's shell, outside the unit's
// cgroup. Naming that process would take the unit away from the daemon systemd
// is supervising — it drops the main pid, kills the cgroup and restarts — so
// the announcement must be limited to a predecessor that is our own parent.
func TestAReloadSuccessorDoesNotClaimTheUnit(t *testing.T) {
	rec := stubNotifications(t, true)
	sock := shortSocket(t, "ghmon-sd-reload-*.sock")

	predecessor, err := ipc.Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = predecessor.Close() })
	sdSupervisedFn = func() bool { return false } // spawned from the operator's session

	announceSuccessor(sock, &rec.stderr)

	order, _ := rec.snapshot()
	assert.Empty(t, order, "a client-spawned successor must not claim the unit")
}

// TestColdStartNamesNoSuccessor pins the other half: an ordinary start says
// nothing about MAINPID. The manager already has it — and after a runtime-copy
// re-exec it is still the PID the manager forked.
func TestColdStartNamesNoSuccessor(t *testing.T) {
	rec := stubNotifications(t, true)
	dir := t.TempDir()

	absent := filepath.Join(dir, "absent.sock")
	announceSuccessor(absent, &rec.stderr)

	// A stale socket file left by a crashed daemon is not a predecessor either.
	stale := filepath.Join(dir, "stale.sock")
	require.NoError(t, os.WriteFile(stale, []byte("not a socket"), 0o600))
	announceSuccessor(stale, &rec.stderr)

	order, _ := rec.snapshot()
	assert.Empty(t, order, "an ordinary start must not claim the unit")
}

// TestNothingNotifiedWithoutAManager verifies an unsupervised daemon pays
// nothing: with the manager disabled, not one notification is attempted even
// when a predecessor is live.
func TestNothingNotifiedWithoutAManager(t *testing.T) {
	rec := stubNotifications(t, false)
	sock := shortSocket(t, "ghmon-sd-off-*.sock")
	predecessor, err := ipc.Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = predecessor.Close() })

	announceSuccessor(sock, &rec.stderr)
	announceReady(&rec.stderr)
	announceStopping(&rec.stderr)

	order, _ := rec.snapshot()
	assert.Empty(t, order)
}

// TestDaemonReportsReadyThenStopping runs the real daemon command: a
// supervised start must report readiness once it serves — a Type=notify unit
// kills the daemon otherwise — and report the clean shutdown when its manager
// signals it.
func TestDaemonReportsReadyThenStopping(t *testing.T) {
	rec := stubNotifications(t, true)
	sock := shortSocket(t, "ghmon-sd-daemon-*.sock")

	stop, _ := startDaemonCommand(t, sock, "10")
	name, _ := probeDaemonHello(t, sock)
	stop()

	assert.Equal(t, DaemonBackendName, name)
	order, _ := rec.snapshot()
	assert.Equal(t, []string{"READY", "STOPPING"}, order,
		"a supervised daemon reports readiness before it is stopped, and never claims the unit on a cold start")
}

// TestNotificationSendFailuresAreNotFatal pins that a manager socket that
// cannot be written is logged and survived: polling must never depend on a
// supervisor.
func TestNotificationSendFailuresAreNotFatal(t *testing.T) {
	rec := stubNotifications(t, true)
	sdReadyFn = func() error { return assert.AnError }
	sdStoppingFn = func() error { return assert.AnError }

	announceReady(&rec.stderr)
	announceStopping(&rec.stderr)

	assert.Contains(t, rec.stderr.String(), "readiness")
	assert.Contains(t, rec.stderr.String(), "shutdown")
}

// TestSubdaemonsCannotSpeakForTheUnit covers the env strip: a sub-daemon is a
// gh-monitor process in the same environment, and one that reported READY=1
// would mark the unit started while the daemon that owns the socket is still
// coming up.
func TestSubdaemonsCannotSpeakForTheUnit(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/run/user/1000/systemd/notify")

	sockDir := t.TempDir()
	l := subdaemonLauncherFn(nil, os.Stderr, sockDir)
	env := l.ChildEnv(subdaemon.Entry{Name: "broker-subscriber", Cmd: []string{"/bin/true"}})

	for _, kv := range env {
		require.False(t, strings.HasPrefix(kv, "NOTIFY_SOCKET="),
			"a sub-daemon must not inherit NOTIFY_SOCKET: %v", env)
	}
	// The redirect it does need must survive the strip.
	assert.Contains(t, env, "GH_MONITOR_SOCK="+mux.SocketPath(sockDir, "broker-subscriber"))
}
