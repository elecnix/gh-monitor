package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/cursor"
	"github.com/elecnix/gh-monitor/internal/ghcli"
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
	cmd.Flags().StringVar(&opts.Viewer, "viewer", "", "GitHub login to classify readiness by (default: authenticated user)")
	addBackendFlags(cmd, &opts.Backend)
}

type monitorOptions struct {
	Repo          string
	Pull          int
	Ref           string
	Commit        string
	Issue         int
	RunID         int
	Selector      string
	Interval      int
	Timeout       int
	IgnoredBots   string
	Events        string
	Annotations   string
	Once          bool
	Text          bool
	Instance      string
	FromBeginning bool
	Viewer        string
	Backend       backendOptions
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
	// Repo-only (--repo without any other target) uses the readiness view.
	repoOnly := o.Repo != "" && o.Selector == "" && o.Pull == 0 && o.Ref == "" && o.Commit == "" && o.Issue == 0 && o.RunID == 0
	if repoOnly {
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
		// Repo-only: use the readiness view instead of the old repo-monitor.
		return runReadiness(cmd, opts)
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

	// Wire cursor I/O for named instances (issue #32). Repo targets use the
	// cursor's Position (a createdAt timestamp); PR and issue targets use the
	// cursor's Snapshot (a JSON-serialised PRStatus or IssueStatus baseline).
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
			runOpts.CursorSnapshot = c.Snapshot
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "gh-monitor: cursor load error: %v\n", err)
		}
		// AdvanceCursor persists the repo cursor after each poll.
		runOpts.AdvanceCursor = func(position string) {
			c := cursor.Cursor{
				Instance: opts.Instance,
				Owner:    identity.Owner,
				Repo:     identity.Repo,
				Position: position,
				LastSeen: time.Now(),
			}
			if err := store.Save(c); err != nil {
				fmt.Fprintf(os.Stderr, "gh-monitor: cursor save error: %v\n", err)
			}
		}
		// SaveSnapshot persists the PR/issue baseline after each successful poll.
		runOpts.SaveSnapshot = func(snapshotJSON string) {
			// Load the current cursor to preserve any Position field (repo mode
			// may have a Position set that we should not clobber).
			c := cursor.Cursor{
				Instance: opts.Instance,
				Owner:    identity.Owner,
				Repo:     identity.Repo,
				Snapshot: snapshotJSON,
				LastSeen: time.Now(),
			}
			// If there is an existing Position, carry it forward.
			if existing, err := store.Load(opts.Instance); err == nil && existing.Position != "" {
				c.Position = existing.Position
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

	write := func(n monitor.Notification) {
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

	// --events applies here, at the one boundary every notification crosses,
	// whatever produced it — the in-process loop, the shared daemon, or an
	// external backend.
	emit := func(n monitor.Notification) {
		if runOpts.EventFilter.Allows(n.Type) {
			write(n)
		}
	}

	// Resolve which backend serves this target. The built-in one covers every
	// kind; the shared-poller daemon and any configured external backend layer
	// over it for the kinds they declare.
	target := monitor.TargetOf(identity)
	reg, err := buildRegistry(ctx, &opts.Backend, runOpts, true)
	if err != nil {
		return err
	}
	// The daemon exists so several processes watching one pull request share a
	// single fetch (issue #34). It only makes sense for a continuous watch, so
	// a one-shot read goes straight to the backend that can answer it.
	if !opts.Once {
		attachDaemon(ctx, reg, target, runOpts.Interval)
	}
	source, sourceName, err := reg.SourceFor(target)
	if err != nil {
		return err
	}

	updates, err := source.Watch(ctx, target, backend.WatchOptions{
		Interval:         runOpts.Interval,
		Timeout:          runOpts.Timeout,
		Once:             opts.Once,
		Since:            runOpts.CursorPosition,
		IgnoredAuthors:   runOpts.Prefs.IgnoredBots,
		RepeatUnresolved: runOpts.Prefs.RetriggerComments,
		AnnotationLevels: runOpts.AnnotationLevels.Names(),
	})
	if err != nil {
		return fmt.Errorf("backend %q: %w", sourceName, err)
	}
	// A backend with at-least-once delivery repeats itself; a repeat that
	// reaches the operator is a second notification about one thing.
	dedup := backend.NewDeduper(0)
	for u := range updates {
		if !dedup.Allow(u) {
			continue
		}
		emit(monitor.Render(u, runOpts.Prefs, runOpts.Interval))
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
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

// ---------------------------------------------------------------------------
// Readiness view (issue #31): repo-wide merge-readiness
// ---------------------------------------------------------------------------

// runReadiness runs the repo-wide merge-readiness view. With --once it fetches
// once and exits; without --once it polls on the configured interval.
func runReadiness(cmd *cobra.Command, opts *monitorOptions) error {
	// Resolve the viewer login.
	viewer := resolveViewer(opts.Viewer)

	// Resolve owner/repo from the --repo flag.
	owner, repo := splitRepo(opts.Repo)
	host := os.Getenv("GH_HOST")
	if host == "" {
		host = "github.com"
	}

	svc := &monitor.Service{API: apiClientFactory(host)}
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

	interval := time.Duration(opts.Interval) * time.Second
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}

	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = time.Now().Add(time.Duration(opts.Timeout) * time.Second)
	}

	emit := func(n monitor.Notification) {
		if opts.Text {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, n.Message)
			return
		}
		if err := encodeJSON(cmd, n); err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: %v\n", err)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

		// Fetch the ruleset once per cycle (it's cheap, and rules can change).
		ruleset, rulesetErr := svc.FetchRequiredChecks(owner, repo)
		if rulesetErr != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: ruleset fetch error: %v\n", rulesetErr)
		}

		// Fetch open PRs.
		resp, err := svc.FetchReadiness(owner, repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: readiness fetch error: %v\n", err)
			// Emit a degraded notification.
			report := &monitor.ReadinessReport{
				Owner:           owner,
				Repo:            repo,
				Viewer:          viewer,
				Degraded:        true,
				DegradedMessage: err.Error(),
			}
			emit(monitor.Notification{
				Type:      "readiness",
				PRLabel:   fmt.Sprintf("%s/%s", owner, repo),
				Message:   report.Format(),
				Timestamp: time.Now(),
			})
			// Back off and retry.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
			continue
		}

		report := monitor.ClassifyPRsFull(resp.Repository.PullRequests.Nodes, viewer, ruleset)
		report.Owner = owner
		report.Repo = repo
		report.Sorted()

		// Reconcile counts and warn on mismatch.
		if errMsg := report.Reconcile(); errMsg != "" {
			fmt.Fprintf(os.Stderr, "gh-monitor: %s\n", errMsg)
		}

		emit(monitor.Notification{
			Type:      "readiness",
			PRLabel:   fmt.Sprintf("%s/%s", owner, repo),
			Message:   report.Format(),
			Timestamp: time.Now(),
		})

		if opts.Once {
			return nil
		}

		// Sleep until next poll or deadline.
		d := interval
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil
			}
			if d > remaining {
				d = remaining
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

// resolveViewer returns the viewer login: --viewer flag, $GH_VIEWER env var,
// or the authenticated user from `gh api user`.
func resolveViewer(flagViewer string) string {
	if flagViewer != "" {
		return flagViewer
	}
	if v := os.Getenv("GH_VIEWER"); v != "" {
		return v
	}
	user, err := ghcli.CurrentUser()
	if err != nil {
		return ""
	}
	return user
}

// splitRepo splits "owner/repo" into its components.
func splitRepo(repoArg string) (owner, repo string) {
	parts := strings.SplitN(repoArg, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return repoArg, ""
}

// attachDaemon registers the shared-poller daemon as a backend for pull
// requests, starting one if none is listening and autostart is enabled.
//
// It is best-effort by design: the daemon is an optimisation — several
// processes watching one pull request share a single fetch — and its absence
// must never stop a watch. Every failure path simply leaves the registry as it
// was, so the built-in backend polls in-process instead.
//
// Registering it as a backend rather than special-casing it is what lets an
// explicitly configured external backend still win: it registers after this
// one, and the later registration takes precedence for the kinds it claims.
func attachDaemon(ctx context.Context, reg *backend.Registry, target backend.Target, interval time.Duration) {
	if target.Kind != backend.KindPR {
		return // the shared poller only serves pull requests
	}
	socket := daemonSocketPath()
	if socket == "" {
		return // opted out via GH_MONITOR_DAEMON=0
	}

	if probe, err := ipc.Dial(socket); err == nil {
		// Only a liveness check — leaving it open would strand a server
		// goroutine on a request that never comes.
		_ = probe.Close()
	} else {
		if !daemonAutostart() {
			return
		}
		if err := autostartDaemon(ctx, socket, interval); err != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"gh-monitor: could not start the shared poller (%v); polling in-process\n", err)
			return
		}
	}

	transport, err := remote.ParseEndpoint("unix:" + socket)
	if err != nil {
		return
	}
	provider, err := remote.Connect(ctx, transport)
	if err != nil {
		// The likeliest cause is a daemon left running from a build before
		// this protocol: it holds the socket and waits for the client to speak
		// first, so the handshake times out. Say what to do about it — the
		// alternative is every invocation quietly paying that timeout.
		_, _ = fmt.Fprintf(os.Stderr,
			"gh-monitor: the process holding %s does not speak this backend protocol (%v).\n"+
				"gh-monitor: polling in-process. If it is a daemon from an older build, stop it:\n"+
				"gh-monitor:   pkill -f 'gh monitor daemon'\n", socket, err)
		return
	}
	if err := reg.Use(provider); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gh-monitor: %v; polling in-process\n", err)
	}
}
