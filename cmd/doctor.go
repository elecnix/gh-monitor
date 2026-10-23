package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/sdnotify"
)

// doctorMinAge is how old an unattached watcher must be before --reap will
// stop it. A watcher that has just launched has not attached yet; killing it
// would make `gh monitor doctor --reap` a race against every client starting
// up.
var doctorMinAge = 5 * time.Minute

// newDoctorCommand builds `gh monitor doctor`: it compares what the resident
// daemon is serving against what is actually running on this machine.
//
// The comparison is the point. A daemon's own log describes its own pollers,
// so a fleet of watchers that never attached to it — started by sessions that
// have since exited, or by an older build that fell back to polling in
// process — is invisible in every log gh-monitor writes. On this machine that
// invisibility cost two days and, at its peak, the whole GraphQL budget of
// forty-two pollers (issue #193).
func newDoctorCommand() *cobra.Command {
	var (
		socket string
		reap   bool
		minAge time.Duration
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Compare what the daemon serves with what is actually running",
		Long: `Report the resident daemon and every gh-monitor process running on this
machine, and say which watchers are polling outside the daemon.

A watcher outside the daemon shares nothing: it does not appear in the
daemon's log, it is not slowed by broker coverage, and it keeps its own
GitHub polling budget. Enough of them exhaust the account's GraphQL quota and
break every other gh workflow that needs it.

  gh monitor doctor           report, and exit non-zero if a watcher polls alone
  gh monitor doctor --reap    also stop unattached watchers older than --min-age

The process scan needs Linux /proc; the daemon half works anywhere.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if socket == "" {
				socket = ipc.DefaultSocketPath()
			}
			out := cmd.OutOrStdout()
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			info, infoErr := handoffInfoFn(ctx, socket)
			switch {
			case infoErr == nil:
				_, _ = fmt.Fprintf(out, "daemon: pid %d serving %s (started %s)\n",
					info.PID, info.Socket, info.StartedAt.Format(time.RFC3339))
				features := []string{}
				if info.Supervised {
					features = append(features, "supervised")
				}
				if info.SocketActivated {
					features = append(features, "socket-activated")
				}
				if info.InvocationID != "" {
					features = append(features, "INVOCATION_ID "+info.InvocationID)
				}
				if len(features) > 0 {
					_, _ = fmt.Fprintf(out, "daemon: %s\n", strings.Join(features, ", "))
				}
				_, _ = fmt.Fprintf(out, "daemon: %d connected client(s)\n", len(info.Clients))
				for _, c := range info.Clients {
					_, _ = fmt.Fprintf(out, "  client pid %d: %s since %s\n",
						c.PID, describeTargets(c), c.Since.Format("15:04:05"))
				}
			case errors.Is(infoErr, handoff.ErrUnsupported):
				_, _ = fmt.Fprintf(out, "daemon: the resident daemon predates `daemon-info`, so its clients cannot be listed\n")
			default:
				if _, err := ipc.Dial(socket); err != nil {
					_, _ = fmt.Fprintf(out, "daemon: none listening on %s\n", socket)
				} else {
					_, _ = fmt.Fprintf(out, "daemon: listening on %s but did not answer (%v)\n", socket, infoErr)
				}
			}
			// Only a daemon that answered can be compared: an unanswered
			// query leaves Info at its zero value, and warning from that would
			// mean "the daemon said nothing" was read as "the daemon is not
			// supervised".
			if infoErr == nil && sdnotify.Enabled() && !info.Supervised {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: this shell has NOTIFY_SOCKET set but the resident daemon reports no service manager\n")
			}

			procs, scanErr := scanWatchers()
			if scanErr != nil {
				_, _ = fmt.Fprintf(out, "processes: %v\n", scanErr)
				return nil
			}
			// The daemon's client list is the only evidence a watcher is
			// attached. Without it — no daemon, an old daemon, one that did not
			// answer — every watcher would look unattached, and --reap would
			// stop a healthy fleet. So the comparison, and the killing, need
			// an answer; their absence is reported, never guessed.
			attached := map[int]bool{}
			for _, c := range info.Clients {
				attached[c.PID] = true
			}

			var unlinked []watcherProcess
			_, _ = fmt.Fprintf(out, "processes: %d gh-monitor watcher(s) running\n", len(procs))
			for _, p := range procs {
				state := "attached to the daemon"
				switch {
				case attached[p.PID]:
				case infoErr != nil:
					// No client list to compare against, so this is unknown,
					// not unattached — and it is not reaped.
					state = "attached? the daemon did not report its clients"
				case p.Age < doctorMinAge:
					state = "just started (not attached yet)"
				default:
					state = "NOT ATTACHED — polling on its own"
					unlinked = append(unlinked, p)
				}
				_, _ = fmt.Fprintf(out, "  pid %d (%s): %s\n", p.PID, p.Age.Round(time.Second), state)
				_, _ = fmt.Fprintf(out, "    %s\n", p.Describe)
			}

			// Nothing to compare against is not the same as nothing wrong. A
			// silent daemon leaves every watcher unverified, and saying "ok"
			// from that would be the reassurance this command exists to
			// replace.
			if infoErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"cannot verify which watcher(s) the daemon serves (%v); nothing was stopped\n", infoErr)
				if len(procs) == 0 {
					return nil // no watchers to verify either: genuinely nothing to check
				}
				return fmt.Errorf("%d watcher(s) could not be verified against the daemon", len(procs))
			}
			if len(unlinked) == 0 {
				_, _ = fmt.Fprintln(out, "ok: every watcher is served by the daemon")
				return nil
			}
			if !reap {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"%d watcher(s) poll GitHub without the daemon; re-run with --reap to stop those older than %s\n",
					len(unlinked), minAge)
				return fmt.Errorf("%d watcher(s) are polling outside the daemon", len(unlinked))
			}
			reaped, failed, tooYoung := 0, 0, 0
			for _, p := range unlinked {
				if p.Age < minAge {
					tooYoung++
					continue
				}
				if err := p.Stop(); err != nil {
					// A stop that fails is not a stop that came too early: the
					// age gate above already let this one through. Counting it
					// as "younger than --min-age" would send the operator back
					// to wait, when waiting is exactly what cannot help.
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "pid %d: %v\n", p.PID, err)
					failed++
					continue
				}
				_, _ = fmt.Fprintf(out, "stopped pid %d\n", p.PID)
				reaped++
			}
			_, _ = fmt.Fprintf(out, "reaped %d of %d unattached watcher(s)\n", reaped, len(unlinked))
			if failed > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"%d watcher(s) could not be stopped (see the errors above); they are still polling outside the daemon\n", failed)
			}
			if tooYoung > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"%d watcher(s) are younger than %s, so re-run --reap once they are older\n", tooYoung, minAge)
			}
			if failed+tooYoung > 0 {
				return fmt.Errorf("%d watcher(s) are still polling outside the daemon", failed+tooYoung)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "Daemon Unix socket path (default: $GH_MONITOR_SOCK or $XDG_RUNTIME_DIR/gh-monitor.sock)")
	cmd.Flags().BoolVar(&reap, "reap", false, "Stop unattached watchers older than --min-age")
	cmd.Flags().DurationVar(&minAge, "min-age", doctorMinAge, "How old an unattached watcher must be before --reap stops it")
	return cmd
}

// describeTargets renders what a connected client asked for.
func describeTargets(c handoff.ClientInfo) string {
	if len(c.Targets) == 0 {
		return "(probing)"
	}
	parts := make([]string, 0, len(c.Targets))
	for _, t := range c.Targets {
		if t.Number > 0 {
			parts = append(parts, fmt.Sprintf("%s/%s#%d", t.Owner, t.Repo, t.Number))
		} else {
			parts = append(parts, fmt.Sprintf("%s/%s", t.Owner, t.Repo))
		}
	}
	if !c.Watch {
		parts = append(parts, "(read)")
	}
	return strings.Join(parts, ", ")
}

// watcherProcess is one gh-monitor process found on the machine.
type watcherProcess struct {
	PID      int
	Age      time.Duration
	Describe string
	// stop terminates the process; a package-level field so tests need no
	// real signals.
	Stop func() error
}

// handoffInfoFn asks the resident daemon to describe itself. A variable so a
// test can describe a daemon without one running.
var handoffInfoFn = handoff.Info

// scanWatchers lists the gh-monitor watcher processes running on this
// machine. It is a variable so a test can describe a fleet without one.
var scanWatchers = func() ([]watcherProcess, error) {
	return listWatchers(os.Getpid())
}
