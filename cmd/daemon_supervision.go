// Supervision of the resident daemon: how it obtains its listening socket,
// how it announces a takeover to a service manager, and how it starts its own
// successor (issue #193).

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/activation"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/elecnix/gh-monitor/internal/sdnotify"
)

// daemonInfo is what one daemon start knows about the process it belongs to.
type daemonInfo struct {
	// invocationID is the service manager's id for this start
	// ($INVOCATION_ID). Two processes sharing it are the same service
	// instance; that is what tells a legitimate successor from one spawned
	// outside the unit, which the unit's teardown would kill.
	invocationID string
	// supervised reports that a service manager is listening for
	// notifications, so a takeover can be announced.
	supervised bool
	// version is this binary's own report, for diagnostics.
	version string
}

// daemonRuntime is the state of one daemon start that outlives its socket
// acquisition.
type daemonRuntime struct {
	socket string
	// socketActivated reports that the listening socket came from the
	// service manager, which then owns its path: the daemon must never
	// unlink it, and never has to hand it over.
	socketActivated bool
	// activated is the listener the service manager passed, once adopted.
	activated net.Listener
	// announced reports that MAINPID has been sent for this process, so a
	// failed takeover knows it has something to undo.
	announced bool
	// inheritedActivated reports that the socket we adopted belongs to a
	// service manager. A successor does not receive the activated descriptor
	// itself (the fd pass hands it the listening socket), so it has to learn
	// this from the predecessor — or it would unlink a path it does not own on
	// its own shutdown.
	inheritedActivated bool
	info               daemonInfo
	interval           time.Duration
}

// openSocket gives the daemon its listening socket, preferring the one the
// service manager passed (socket activation) and otherwise binding or
// adopting the path.
//
// Activation is tried first because it makes the socket independent of the
// daemon's lifetime: a restart of the unit is then invisible to a client's
// connect(), and a handoff only has to carry watching state.
func (d *daemonRuntime) openSocket(ctx context.Context, cmd *cobra.Command) (net.Listener, *hub.State, error) {
	if d.activated != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: using the activated socket %s\n", d.socket)
		return d.activated, nil, nil
	}
	return d.listenOrAdopt(ctx)
}

// adoptActivated takes the socket the service manager passed, if any, and
// records the path it is really bound to.
//
// It runs before anything derived from the path is set up — sub-daemon private
// sockets, the removal-on-shutdown rule — because with socket activation the
// service manager decides where the socket lives, and every path this daemon
// derives must be the one clients actually connect to.
func (d *daemonRuntime) adoptActivated(ctx context.Context, cmd *cobra.Command) error {
	env := activation.ProcessEnv()
	env.File = activationProcessFile
	listeners, err := activation.Listeners(env)
	if err != nil {
		return fmt.Errorf("adopt the socket passed by the service manager: %w", err)
	}
	if len(listeners) == 0 {
		return nil
	}
	for _, extra := range listeners[1:] {
		// gh-monitor serves exactly one socket. Serving on the first and
		// leaving the others queued would be a half-open daemon.
		_ = extra.Close()
		return fmt.Errorf("activation: %d listeners were passed, gh-monitor serves exactly one", len(listeners))
	}
	l := listeners[0]
	if ul, ok := l.(*net.UnixListener); ok {
		if path := ul.Addr().String(); path != "" && path != d.socket {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: serving the activated socket at %s (not %s)\n", path, d.socket)
			d.socket = path
		}
	}
	// A connect alone is no evidence of a daemon here: the service manager
	// holds the listening end even when nothing is serving, so a second start
	// would otherwise queue clients behind a daemon that never answers.
	if info, live := probeLiveDaemon(ctx, d.socket); live && info.PID != os.Getpid() {
		_ = l.Close()
		return fmt.Errorf("%w: %s is already served by pid %d", ipc.ErrLiveDaemon, d.socket, info.PID)
	}
	d.socketActivated = true
	d.activated = l
	return nil
}

// runningVersion reports this binary's version, for the daemon's Info.
func runningVersion() string {
	v := version
	if v == "" {
		v = devel
	}
	return v
}

// listenOrAdopt binds the daemon socket, or adopts a live daemon's. Between
// the two, a supervised takeover announces itself to the service manager
// before the socket changes hands (issue #193): systemd must already know
// that this process is the service's main process when the predecessor exits,
// or Restart=always plus KillMode=control-group turn the handoff into a
// restart that kills the successor along with the predecessor.
func (d *daemonRuntime) listenOrAdopt(ctx context.Context) (net.Listener, *hub.State, error) {
	l, err := ipc.Listen(d.socket)
	if err == nil {
		return l, nil, nil
	}
	if !errors.Is(err, ipc.ErrLiveDaemon) || ctx == nil {
		return nil, nil, err
	}
	adopted, state, aerr := handoff.Adopt(ctx, d.socket, handoff.Options{
		BeforeFD: func(pre handoff.DaemonInfo) error {
			if serr := d.supervisorError(pre); serr != nil {
				return serr
			}
			d.inheritedActivated = pre.SocketActivated
			if d.info.supervised {
				if nerr := sdnotify.MainPID(os.Getpid()); nerr != nil {
					return fmt.Errorf("announce the takeover to the service manager: %w", nerr)
				}
				d.announced = true
			}
			return nil
		},
		AbortFD: func(pre handoff.DaemonInfo) {
			// The fd pass failed and the predecessor is still serving, so the
			// service must point back at it: a MAINPID naming a successor that
			// never took over is how a healthy daemon gets killed on its next
			// restart.
			if d.announced && pre.PID != 0 {
				_ = sdnotify.MainPID(pre.PID)
			}
		},
	})
	if aerr != nil {
		return nil, nil, fmt.Errorf("%w (upgrade handoff failed: %v)", err, aerr)
	}
	return adopted, &state, nil
}

// listenOrAdopt is listenOrAdopt without a runtime: the unsupervised,
// no-notification path, kept for callers that have no service manager in
// play (and for tests that exercise the handoff seam directly).
func listenOrAdopt(ctx context.Context, socket string) (net.Listener, *hub.State, error) {
	d := &daemonRuntime{socket: socket, info: daemonInfo{supervised: sdnotify.Enabled()}}
	return d.listenOrAdopt(ctx)
}

// supervisorError explains why a handoff was refused. Both cases are the same
// failure seen from different sides: the successor would end up outside the
// service, and the unit's teardown (KillMode=control-group) would kill it as
// soon as the predecessor exits — a restart disguised as a handoff.
func (d *daemonRuntime) supervisorError(pre handoff.DaemonInfo) error {
	if pre.InvocationID != "" && pre.InvocationID != d.info.invocationID {
		if d.info.invocationID == "" {
			return fmt.Errorf("the resident daemon on %s is supervised by a service manager and this process is not part of that service; taking over would leave a daemon the unit kills. Restart the unit, or run 'gh monitor reload' against a daemon that can spawn its own successor", d.socket)
		}
		return fmt.Errorf("the resident daemon on %s belongs to another service instance (INVOCATION_ID %s, this process has %s); refusing to take over", d.socket, pre.InvocationID, d.info.invocationID)
	}
	return nil
}

// listEnvWithoutActivation returns this process's environment with the
// socket-activation and reload variables removed. A child must never inherit
// descriptors the service manager passed to its parent: they describe
// nothing in the child.
func listEnvWithoutActivation() []string {
	drop := map[string]bool{
		activation.FDsEnv:   true,
		activation.PIDEnv:   true,
		activation.NamesEnv: true,
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// spawnSuccessor starts this daemon's successor and returns its pid. The
// successor inherits this process's environment, which is the point: it stays
// inside the unit's cgroup and carries the same INVOCATION_ID, so systemd
// follows the handoff instead of treating it as an exit.
//
// The installed binary is preferred when the runtime-copy launcher recorded
// one, so a reload after an upgrade serves the new version.
func (d *daemonRuntime) spawnSuccessor() (int, error) {
	bin := os.Getenv(reexec.InstalledBinEnv)
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return 0, fmt.Errorf("locate executable: %w", err)
		}
		bin = exe
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	// The interval flag is passed only when a real cadence was resolved. Its
	// own default is the built-in cadence, and announcing "--interval 0" for a
	// runtime built without one would read as "never poll" to anyone looking
	// at the process table.
	args := []string{"daemon", "--socket", d.socket}
	if d.interval >= 10*time.Second {
		args = append(args, "--interval", strconv.Itoa(int(d.interval.Seconds())))
	}
	c := exec.Command(bin, args...)
	c.Stdin = devnull
	c.Stdout = devnull
	c.Stderr = devnull
	c.Env = listEnvWithoutActivation()
	detachProcess(c)
	if err := c.Start(); err != nil {
		return 0, fmt.Errorf("spawn successor daemon: %w", err)
	}
	pid := c.Process.Pid
	_ = c.Process.Release()
	return pid, nil
}

// activationProcessFile wraps a passed descriptor. A variable so a test can
// simulate socket activation without claiming fd 3 in a running test binary.
var activationProcessFile = os.NewFile

// daemonInfoJSON renders the daemon's Info for the OpInfo response.
func daemonInfoJSON(ctx context.Context, srv *daemonServer, started time.Time) (json.RawMessage, error) {
	info := handoff.DaemonInfo{
		PID:             os.Getpid(),
		Socket:          srv.socket,
		Version:         srv.info.version,
		InvocationID:    srv.info.invocationID,
		Supervised:      srv.info.supervised,
		SocketActivated: srv.socketActivated,
		StartedAt:       started,
		Clients:         srv.clients.snapshotExcept(clientEntryFrom(ctx)),
	}
	return json.Marshal(info)
}

// recordingSource records what a client asks for, so the daemon can report
// what it is serving. It records and passes through: a client whose request
// cannot be recorded is still served.
type recordingSource struct {
	inner backend.Source
	reg   *clientRegistry
	entry *clientEntry
}

func (s recordingSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.reg.observe(s.entry, opts, t)
	return s.inner.Watch(ctx, t, opts)
}

// errNoSuccessor reports that the daemon could not start one, which the
// reload client turns into advice rather than a silent no-op.
var errNoSuccessor = errors.New("the resident daemon could not start its successor")

// waitForNewDaemon waits until the socket answers under a pid other than
// oldPID, which is how a reload knows the successor is serving.
func waitForNewDaemon(ctx context.Context, socket string, oldPID int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		info, err := handoff.Info(ctx, socket)
		if err == nil && info.PID != 0 && info.PID != oldPID {
			return info.PID, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("no successor answered")
			}
			return 0, fmt.Errorf("no successor took over %s within %s (%v)", socket, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// probeLiveDaemon reports whether a daemon is actually serving on socket —
// not merely bound. A socket-activated path connects successfully even with
// no daemon running (the service manager holds the listening end), so a
// connect is not evidence of a daemon; an answered hello is.
func probeLiveDaemon(ctx context.Context, socket string) (handoff.DaemonInfo, bool) {
	c, err := ipc.Dial(socket)
	if err != nil {
		return handoff.DaemonInfo{}, false
	}
	_ = c.Close()
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	info, err := handoff.Info(pctx, socket)
	if err != nil {
		return handoff.DaemonInfo{}, false
	}
	return info, true
}
