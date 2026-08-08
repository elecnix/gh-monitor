package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/cursor"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

func addMonitorFlags(cmd *cobra.Command, opts *monitorOptions) {
	cmd.Flags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.Flags().IntVar(&opts.Pull, "pr", 0, "Pull request number")
	cmd.Flags().StringVar(&opts.Ref, "ref", "", "Branch or ref to monitor (CI checks only)")
	cmd.Flags().StringVar(&opts.Commit, "commit", "", "Commit SHA to monitor (CI checks only)")
	cmd.Flags().IntVar(&opts.Issue, "issue", 0, "Issue number to monitor")
	cmd.Flags().IntVar(&opts.RunID, "run-id", 0, "GitHub Actions workflow run id to monitor (watches a single run until it completes)")
	cmd.Flags().IntVarP(&opts.Interval, "interval", "i", 60, "Polling interval in seconds (min 10)")
	cmd.Flags().IntVarP(&opts.Timeout, "timeout", "t", 0, "Maximum watch time in seconds (0 = run until merged/closed)")
	cmd.Flags().StringVar(&opts.IgnoredBots, "ignored-bots", "", "Comma-separated author logins whose general comments are ignored")
	cmd.Flags().StringVar(&opts.Events, "events", "", "Comma-separated list of event kinds to emit (suppresses all others); omit to emit everything")
	cmd.Flags().StringVar(&opts.Events, "only-events", "", "Alias for --events")
	cmd.Flags().StringVar(&opts.Annotations, "annotation-levels", "", "Comma-separated annotation levels to surface: notice, warning, failure, or none (default: warning,failure)")
	cmd.Flags().BoolVar(&opts.Once, "once", false, "Fetch once, emit the current actionable state, and exit")
	cmd.Flags().BoolVar(&opts.Text, "text", false, "Emit the rendered message per event instead of NDJSON")
	cmd.Flags().StringVar(&opts.Instance, "instance", "", "Named instance identifier for resumable cursor (issue #32)")
	cmd.Flags().BoolVar(&opts.FromBeginning, "from-beginning", false, "Replay the full backlog, ignoring any cursor (new named instances start at 'now' by default)")
}

type monitorOptions struct {
	Repo        string
	Pull        int
	Ref         string
	Commit      string
	Issue       int
	RunID       int
	Selector    string
	Interval    int
	Timeout     int
	IgnoredBots string
	Events      string
	Annotations string
	Once        bool
	Text        bool
	Instance    string
	FromBeginning bool
}

func (o *monitorOptions) Validate() error {
	if o.Interval < 10 {
		return errors.New("--interval must be at least 10 seconds")
	}
	if o.Timeout < 0 {
		return errors.New("--timeout must be a non-negative integer")
	}

	// Count how many target kinds are specified.
	targets := 0
	if o.Selector != "" || o.Pull > 0 {
		targets++
	}
	if o.Ref != "" {
		targets++
	}
	if o.Commit != "" {
		targets++
	}
	if o.Issue > 0 {
		targets++
	}
	if o.RunID > 0 {
		targets++
	}
	// Repo-only (--repo without any other target) is its own target kind.
	repoOnly := o.Repo != "" && o.Selector == "" && o.Pull == 0 && o.Ref == "" && o.Commit == "" && o.Issue == 0 && o.RunID == 0
	if repoOnly {
		// Repo monitoring: valid on its own; no conflict with other targets.
		return nil
	}
	if targets > 1 {
		return errors.New("--ref, --commit, --issue, --run-id, and a PR selector are mutually exclusive")
	}
	if targets == 0 {
		return errors.New("pull request number or URL is required")
	}

	return nil
}

func runMonitor(cmd *cobra.Command, opts *monitorOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	inferRepo(&opts.Repo)

	var identity resolver.Identity
	var err error

	if opts.Ref != "" {
		identity, err = resolver.ResolveRef(opts.Ref, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.Commit != "" {
		identity, err = resolver.ResolveCommit(opts.Commit, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.Issue > 0 {
		identity, err = resolver.ResolveIssue(opts.Issue, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.RunID > 0 {
		identity, err = resolver.ResolveRun(opts.RunID, opts.Repo, os.Getenv("GH_HOST"))
	} else if opts.Repo != "" && opts.Selector == "" && opts.Ref == "" && opts.Commit == "" && opts.Issue == 0 && opts.RunID == 0 {
		identity, err = resolver.ResolveRepo(opts.Repo, os.Getenv("GH_HOST"))
	} else {
		inferPR(opts.Selector, &opts.Pull)
		selector, normErr := resolver.NormalizeSelector(opts.Selector, opts.Pull)
		if normErr != nil {
			return normErr
		}
		identity, err = resolver.Resolve(selector, opts.Repo, os.Getenv("GH_HOST"))
	}
	if err != nil {
		return err
	}

	p, err := prefs.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-monitor: using default templates (%v)\n", err)
	}
	for _, bot := range strings.Split(opts.IgnoredBots, ",") {
		if b := strings.TrimSpace(bot); b != "" {
			p.IgnoredBots = append(p.IgnoredBots, b)
		}
	}

	svc := &monitor.Service{API: apiClientFactory(identity.Host)}
	// Wire the failed-run log fetcher (issue #19) so run-completed
	// notifications for failed runs embed a `gh run view --log-failed` snippet.
	if c, ok := svc.API.(*ghcli.Client); ok {
		svc.FailedRunLogsFn = c.FailedRunLogs
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	runOpts := monitor.RunOptions{
		Identity:      identity,
		Prefs:         p,
		Interval:      time.Duration(opts.Interval) * time.Second,
		Timeout:       time.Duration(opts.Timeout) * time.Second,
		Instance:      opts.Instance,
		FromBeginning: opts.FromBeginning,
	}

	// Wire cursor I/O for named instances (issue #32). The cursor is only
	// meaningful for repo targets, but we thread it unconditionally — other
	// targets ignore the fields.
	if opts.Instance != "" {
		prefsPath, err := prefs.ConfigPath("")
		if err != nil {
			return fmt.Errorf("resolve config path: %w", err)
		}
		cfgDir := filepath.Dir(prefsPath)
		store, err := cursor.NewDiskStore(cfgDir)
		if err != nil {
			return fmt.Errorf("create cursor store: %w", err)
		}
		// Load the existing cursor, if any.
		if c, err := store.Load(opts.Instance); err == nil {
			runOpts.CursorPosition = c.Position
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "gh-monitor: cursor load error: %v\n", err)
		}
		// AdvanceCursor persists the cursor after each poll.
		runOpts.AdvanceCursor = func(position string) {
			c := cursor.Cursor{
				Instance: opts.Instance,
				Owner:    identity.Owner,
				Repo:    identity.Repo,
				Position: position,
				LastSeen: time.Now(),
			}
			if err := store.Save(c); err != nil {
				fmt.Fprintf(os.Stderr, "gh-monitor: cursor save error: %v\n", err)
			}
		}
	}

	// --events / --only-events: a per-event-kind allowlist. When set, only the
	// listed kinds are emitted; everything else is suppressed. An unknown kind
	// is rejected loudly so a typo doesn't silently mute what the caller wanted.
	if strings.TrimSpace(opts.Events) != "" {
		filter, err := monitor.ParseEventFilter(opts.Events)
		if err != nil {
			return err
		}
		runOpts.EventFilter = filter
	}

	// --annotation-levels: a per-annotation-level filter applied at snapshot
	// time. Omitted (nil) → default (warning + failure).
	if strings.TrimSpace(opts.Annotations) != "" {
		levels, err := monitor.ParseAnnotationLevels(opts.Annotations)
		if err != nil {
			return err
		}
		runOpts.AnnotationLevels = levels
	}

	emit := func(n monitor.Notification) {
		if opts.Text {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, monitor.LinkifyText(n))
			if n.Detail != "" {
				for _, line := range strings.Split(n.Detail, "\n") {
					_, _ = fmt.Fprintf(out, "  %s\n", line)
				}
			}
			return
		}
		if err := encodeJSON(cmd, n); err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: %v\n", err)
		}
	}

	// If a shared-poller daemon is running, stream from it so multiple
	// processes watching the same PR share one fetch (issue #34). --once and
	// non-PR targets always use the in-process loop; the daemon path is for
	// continuous PR monitoring only.
	if socket := daemonSocketPath(); !opts.Once && identity.Target == "pr" && socket != "" {
		err := streamFromDaemonAndEmit(ctx, socket, runOpts, emit)
		if err == nil {
			return nil
		}
		if !ipc.IsAbsent(err) {
			return err
		}
		// Socket missing: no daemon running — fall through to in-process polling.
	}

	if opts.Once {
		return monitor.Once(ctx, svc, runOpts, emit)
	}

	if err := monitor.Run(ctx, svc, runOpts, emit); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil // clean shutdown on signal
		}
		return err
	}
	return nil
}

// daemonSocketPath returns the daemon socket path, or "" when the user has
// opted out via GH_MONITOR_DAEMON=0. It honours $GH_MONITOR_SOCK for tests.
func daemonSocketPath() string {
	if os.Getenv("GH_MONITOR_DAEMON") == "0" {
		return ""
	}
	return ipc.DefaultSocketPath()
}

// streamFromDaemonAndEmit connects to a running daemon, sends a subscribe
// request for runOpts, and pipes the streamed notifications through emit
// (honouring --text) until the daemon closes the stream or ctx is cancelled.
// When no daemon is listening and autostart is enabled (the default), it
// spawns one detached and waits for it to come up first.
func streamFromDaemonAndEmit(ctx context.Context, socket string, runOpts monitor.RunOptions, emit func(monitor.Notification)) error {
	req := ipc.Subscribe{
		Target:   "pr",
		Identity: runOpts.Identity,
		Prefs:    runOpts.Prefs,
		Interval: int(runOpts.Interval.Seconds()),
		Timeout:  int(runOpts.Timeout.Seconds()),
	}

	// Auto-start: if no daemon is listening, spawn one detached and wait for
	// it to accept. Best-effort — on failure fall back to in-process polling.
	if daemonAutostart() {
		if c, err := ipc.Dial(socket); err == nil {
			_ = c.Close() // daemon already up; don't spawn a second one
		} else if spawnErr := autostartDaemon(ctx, socket, runOpts.Interval); spawnErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "gh-monitor: could not autostart daemon (%v); using in-process polling\n", spawnErr)
			return os.ErrNotExist // so runMonitor falls back to in-process polling
		}
	}

	pr, pw := io.Pipe()
	go func() {
		err := streamFromDaemon(ctx, socket, req, pw)
		_ = pw.CloseWithError(err)
	}()
	r := bufio.NewReader(pr)
	dec := newNDJSONDecoder(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := dec.Decode()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			// A pipe write error from a closed daemon surfaces here; treat a
			// closed pipe as a clean end of stream.
			if errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return err
		}
		emit(n)
	}
}
