package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/ipc"
)

// reloadWaitReadyTimeout bounds how long reload waits for the successor
// daemon to serve the socket — the same bound autostart uses.
const reloadWaitReadyTimeout = 8 * time.Second

// reloadTakeoverTimeout bounds the wait for a daemon-started successor to
// answer under a new pid. It is longer than the readiness bound because the
// successor reads preferences and re-adopts the watching state before it
// serves, and a reload of a busy daemon must not be called a failure while it
// is in fact succeeding.
const reloadTakeoverTimeout = 20 * time.Second

// newReloadCommand builds `gh monitor reload`: a state-preserving restart of
// the resident daemon so it re-reads the global preferences file.
//
// There is deliberately no in-process settings refresh: the daemon's cadence,
// ceiling, and pause policy are read once at start, and a live mutation would
// need every poller to re-derive its timers mid-flight. Instead, reload spawns
// a successor daemon against the same socket, and the existing upgrade-handoff
// machinery (issue #73) does the rest: the successor receives the old daemon's
// watching state over the socket — watched targets, snapshots, backoff state,
// query tiers, cached rulesets, and each connected watcher's baseline — then
// adopts the listening socket itself and serves in its place having read the
// fresh preferences at start. Nothing is lost, nothing replays, no client sees
// a gap.
func newReloadCommand() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Restart the resident daemon so it re-reads preferences (watching state carries over)",
		Long: `Restart the resident daemon so it re-reads the global preferences file.

The daemon reads its settings (selfUpdate, pollInterval, idlePollCeiling,
pollWhenBrokerHealthy) once at start. After editing them — typically via
'gh monitor prefs set' — run this to apply them now:

  gh monitor prefs set '{"pollInterval": "10m"}'
  gh monitor reload

The restart is state-preserving: the successor daemon takes over from the
resident one through the same in-memory handoff an upgrade uses (issue #73).
Every watched target's snapshot, backoff state, query tier, and cached
ruleset carry across, as does each connected watcher's baseline — no second
first-poll, no replay, no gap on the socket.

If no daemon is running there is nothing to reload: the message says so, and
the settings simply apply at the next start. If the resident daemon predates
the handoff protocol the takeover cannot happen; the old daemon keeps serving
with its current settings until it is stopped by hand.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if socket == "" {
				socket = ipc.DefaultSocketPath()
			}
			// Probe first: reloading with no daemon running is a friendly
			// no-op, not a spawn. The probe is exactly what ipc.Listen uses
			// to distinguish a live daemon from a stale socket file.
			if c, err := ipc.Dial(socket); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"gh-monitor: no resident daemon on %s — settings apply at next start\n", socket)
				return nil
			} else {
				_ = c.Close()
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			// Ask the resident daemon to start its own successor. That is the
			// only way a supervised reload can work: a successor spawned from
			// here lands in this client's cgroup, so a service manager would
			// kill it as soon as the old daemon exits — the handoff would be a
			// restart wearing a handoff's clothes (issue #193).
			before, infoErr := handoff.Info(ctx, socket)
			if infoErr == nil {
				pid, rerr := handoff.Reload(ctx, socket)
				switch {
				case rerr == nil:
					newPID, werr := waitForNewDaemon(ctx, socket, before.PID, reloadTakeoverTimeout)
					if werr != nil {
						return fmt.Errorf("%w (the successor was started as pid %d)", werr, pid)
					}
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"gh-monitor: reloaded — successor daemon (pid %d) serving %s with fresh preferences; watching state carried over\n", newPID, socket)
					return nil
				case errors.Is(rerr, handoff.ErrUnsupported):
					// A daemon that predates daemon-reload can only be handed
					// off from outside. Do that, and say what it costs under a
					// service manager rather than pretending it is seamless.
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"gh-monitor: the resident daemon predates self-reload, so the successor is started from here\n")
				default:
					return fmt.Errorf("ask the resident daemon to reload: %w", rerr)
				}
			}

			// Spawn the successor detached (same path autostart uses). Its
			// listenOrAdopt performs the handoff against the live daemon;
			// this process needs no further involvement.
			if err := spawnDaemonFn(socket, 300); err != nil {
				return fmt.Errorf("spawn successor daemon: %w", err)
			}
			if err := ipc.WaitReady(ctx, socket, reloadWaitReadyTimeout); err != nil {
				return fmt.Errorf("wait for successor daemon: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"gh-monitor: reloaded — successor daemon serving %s with fresh preferences; watching state carried over\n", socket)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "Daemon Unix socket path (default: $GH_MONITOR_SOCK or $XDG_RUNTIME_DIR/gh-monitor.sock)")
	return cmd
}
