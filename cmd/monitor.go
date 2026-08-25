package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"github.com/elecnix/gh-monitor/internal/eventlog"
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
	cmd.Flags().StringVar(&opts.Baseline, "baseline", "", "Commit OID observed before starting a --ref watch; the first poll diffs against it instead of going silent")
	cmd.Flags().StringVar(&opts.Commit, "commit", "", "Commit SHA to monitor (CI checks only)")
	cmd.Flags().IntVar(&opts.Issue, "issue", 0, "Issue number to monitor")
	cmd.Flags().IntVar(&opts.RunID, "run-id", 0, "GitHub Actions workflow run id to monitor (watches a single run until it completes)")
	cmd.Flags().IntVarP(&opts.Interval, "interval", "i", 300, "Polling interval in seconds (min 10)")
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
	Baseline      string
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
	// --baseline seeds a ref watch with an explicitly observed commit OID. It
	// only makes sense for ref targets, and it must not be combined with a
	// named instance: the stored cursor already carries a baseline, and two
	// sources of truth for "what was last seen" reintroduce the race --baseline
	// exists to close. These checks come first so --baseline never silently
	// reroutes into another watch mode.
	if strings.TrimSpace(o.Baseline) != "" && o.Ref == "" {
		return errors.New("--baseline requires --ref")
	}
	if strings.TrimSpace(o.Baseline) != "" && o.Instance != "" {
		return errors.New("--baseline cannot be combined with --instance: the stored cursor already provides the baseline")
	}
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

	// A continuous watch is resident: it keeps the running image mapped for
	// as long as it polls. Launch it from a runtime copy of the binary so the
	// installed file stays free for `gh extension upgrade` to rewrite in
	// place (issue #73). One-shot reads exit immediately and need nothing.
	if !opts.Once {
		if err := maybeReexecFn(); err != nil {
			fmt.Fprintf(os.Stderr,
				"gh-monitor: could not relaunch from a runtime copy (%v); running from the installed binary\n", err)
		}
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

	// --baseline: the caller observed a commit OID before starting this watch
	// and passes it explicitly (re-deriving it at watch time would reopen the
	// race). Resolve it through GitHub now so a short SHA expands to the exact
	// full form ref polls report, check state at observation is captured, and
	// a typo'd or inaccessible SHA fails loudly instead of silently never
	// matching. The resolved snapshot seeds the watch's baseline below.
	seededBaseline := ""
	if strings.TrimSpace(opts.Baseline) != "" {
		status, err := monitor.ResolveRefBaseline(apiClientFactory(os.Getenv("GH_HOST")), identity.Owner, identity.Repo, opts.Baseline)
		if err != nil {
			return err
		}
		b, err := json.Marshal(status)
		if err != nil {
			return fmt.Errorf("--baseline: encode status: %w", err)
		}
		seededBaseline = string(b)
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
		Identity: identity,
		Prefs:    p,
		Interval: time.Duration(opts.Interval) * time.Second,
	}

	// Cursor state for named instances (issue #32). Repo targets use the
	// position (a createdAt timestamp); PR and issue targets use the snapshot
	// (a JSON-serialised PRStatus or IssueStatus baseline). persistFromUpdate
	// is how the daemon path persists cursor state, since the polling happens
	// on the far side of the wire and the client only sees what the updates
	// carry.
	var (
		cursorPosition    string
		cursorSnapshot    string
		persistFromUpdate func(backend.Update)
	)
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
			cursorPosition = c.Position
			cursorSnapshot = c.Snapshot
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "gh-monitor: cursor load error: %v\n", err)
		}
		// advanceCursor persists the repo cursor after each poll.
		advanceCursor := func(position string) {
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
		// saveSnapshot persists the PR/issue baseline after each successful poll.
		saveSnapshot := func(snapshotJSON string) {
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
		// The daemon path persists from the updates it receives: a distilled
		// Status becomes the new baseline snapshot (degraded updates carry no
		// status, so a cursor never advances past events that were never read),
		// and a repo target's Cursor token — the latest createdAt in the polled
		// response — advances the position.
		persistFromUpdate = func(u backend.Update) {
			if u.Status != nil {
				if b, err := json.Marshal(u.Status); err == nil {
					saveSnapshot(string(b))
				}
			}
			if u.Cursor != "" {
				advanceCursor(u.Cursor)
			}
		}
	}

	// --events / --only-events: a per-event-kind allowlist. When set, only the
	// listed kinds are emitted; everything else is suppressed. An unknown kind
	// is rejected loudly so a typo doesn't silently mute what the caller wanted.
	var eventFilter *monitor.EventFilter
	if strings.TrimSpace(opts.Events) != "" {
		filter, err := monitor.ParseEventFilter(opts.Events)
		if err != nil {
			return err
		}
		eventFilter = filter
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
	// whatever produced it — the shared daemon or an external backend.
	emit := func(n monitor.Notification) {
		if eventFilter == nil || eventFilter.Allows(n.Type) {
			write(n)
		}
	}

	// Resolve which backend serves this target. The built-in one covers reads
	// and mutations for every kind; the shared-poller daemon — mandatory for
	// every watch, one-shot included — and any configured external backend
	// layer over it for the kinds they declare.
	target := monitor.TargetOf(identity)
	reg, err := buildRegistry(ctx, &opts.Backend, runOpts, true)
	if err != nil {
		return err
	}
	// An explicitly configured external backend is an authoritative operator
	// choice: it registers after the daemon would and wins for the kinds it
	// declares, so attaching the daemon is skipped entirely — including its
	// autostart. Otherwise the shared poller is mandatory for every continuous
	// watch. A one-shot read skips it too: the built-in backend answers --once
	// with a single in-process fetch (hub.Once), and spawning a daemon per
	// read would buy nothing.
	if !opts.Once && opts.Backend.endpoint() == "" {
		if err := attachDaemon(ctx, reg, target, runOpts.Interval); err != nil {
			return err
		}
	}
	source, sourceName, err := reg.SourceFor(target)
	if err != nil {
		return err
	}

	// A continuous watch gets a ResumeID: if the shared-poller daemon hands
	// off to an upgraded daemon (issue #73), the watcher re-establishes its
	// stream under the same ID and resumes from the baseline it was last
	// shown, instead of replaying what it already reported.
	var resumeID string
	if !opts.Once {
		resumeID = newResumeID()
	}

	watchOpts := backend.WatchOptions{
		Interval:         runOpts.Interval,
		Timeout:          time.Duration(opts.Timeout) * time.Second,
		Once:             opts.Once,
		Since:            cursorPosition,
		IgnoredAuthors:   runOpts.Prefs.IgnoredBots,
		RepeatUnresolved: runOpts.Prefs.RetriggerComments,
		AnnotationLevels: runOpts.AnnotationLevels.Names(),
		Baseline:         cursorSnapshot,
		ResumeID:         resumeID,
	}
	if seededBaseline != "" {
		watchOpts.Baseline = seededBaseline
	}
	// A named repo instance with no cursor yet starts at "now" (issue #32):
	// the daemon polls on the far side of the wire, so the client computes
	// the threshold and passes it via Since.
	if opts.Instance != "" && target.Kind == backend.KindRepo && cursorPosition == "" && !opts.FromBeginning {
		watchOpts.Since = time.Now().UTC().Format(time.RFC3339)
	}
	updates, err := source.Watch(ctx, target, watchOpts)
	if err != nil {
		return fmt.Errorf("backend %q: %w", sourceName, err)
	}
	// A backend with at-least-once delivery repeats itself; a repeat that
	// reaches the operator is a second notification about one thing.
	dedup := backend.NewDeduper(0)

	// Event log (issue #86): when the operator turned it on in the global
	// preferences, every delivered update — from the built-in gh backend,
	// the shared daemon, or an out-of-process broker sub-daemon — is
	// recorded above the backend layer, before rendering. Logging is a
	// witness: a failure disables it with one loud line, never the watch.
	var evlog *eventlog.Writer
	if cfg := runOpts.Prefs.EventLog; cfg != nil {
		dir := cfg.Dir
		if dir == "" {
			dir = DefaultEventLogDir()
		}
		keep := cfg.KeepDays
		if keep <= 0 {
			keep = prefs.DefaultEventLogKeepDays
		}
		w, err := eventlog.New(dir, keep)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"gh-monitor: event log disabled (%v) — set a writable eventLog.dir in preferences\n", err)
		} else {
			evlog = w
			defer func() { _ = w.Close() }()
		}
	}
	evlogFailed := false

	for u := range updates {
		if !dedup.Allow(u) {
			continue
		}
		// The daemon path persists cursor state from what each update carries.
		if persistFromUpdate != nil && sourceName == DaemonBackendName {
			persistFromUpdate(u)
		}
		if evlog != nil && !evlogFailed {
			if err := evlog.Log(u); err != nil {
				evlogFailed = true
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"gh-monitor: event log write failed (%v); logging disabled for this watch\n", err)
			}
		}
		emit(monitor.Render(u, runOpts.Prefs, runOpts.Interval))
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// daemonSocketPath returns the daemon socket path. It honours $GH_MONITOR_SOCK
// for tests.
func daemonSocketPath() string {
	return ipc.DefaultSocketPath()
}

// DefaultEventLogDir is where the event log lives when eventLog.dir is
// unset: the user cache dir's gh-monitor/events (honouring $GH_MONITOR_CACHE
// style overrides the same way the runtime copy does — via the OS cache dir).
func DefaultEventLogDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "gh-monitor", "events")
}

// newResumeID generates the identifier one continuous watch keeps across
// daemon reconnects (issue #73). Cryptographically random so concurrent
// watchers can never collide; hex so it rides any protocol untouched.
func newResumeID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// No entropy is not fatal: an empty ID just means a reconnect after a
		// handoff replays current state instead of resuming a baseline.
		return ""
	}
	return hex.EncodeToString(b)
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

// attachDaemon registers the shared-poller daemon as a backend for the given
// target, starting one if none is listening and autostart is enabled.
//
// The daemon is the only watch path (issue #76): one GitHub fetch shared
// by N watchers, broker/webhook fan-out, tier-shedding. Every watch —
// continuous or one-shot — therefore requires it, and every failure to attach
// is a hard error naming the fix.
//
// Registering the daemon as a backend rather than special-casing it is what
// lets an explicitly configured external backend still win: it registers
// after this one, and the later registration takes precedence for the kinds
// it claims.
func attachDaemon(ctx context.Context, reg *backend.Registry, target backend.Target, interval time.Duration) error {
	socket := daemonSocketPath()

	if probe, err := ipc.Dial(socket); err == nil {
		// Only a liveness check — leaving it open would strand a server
		// goroutine on a request that never comes.
		_ = probe.Close()
	} else {
		if !daemonAutostart() {
			return fmt.Errorf("no shared poller is listening on %s and autostart is disabled (GH_MONITOR_AUTOSTART=0); start one with 'gh monitor daemon'", socket)
		}
		if err := autostartDaemon(ctx, socket, interval); err != nil {
			return fmt.Errorf("could not start the shared poller (%v); start one with 'gh monitor daemon'", err)
		}
	}

	transport, err := remote.ParseEndpoint("unix:" + socket)
	if err != nil {
		return fmt.Errorf("parse daemon endpoint: %w", err)
	}
	provider, err := remote.Connect(ctx, transport)
	if err != nil {
		// The likeliest cause is a daemon left running from a build before
		// this protocol: it holds the socket and waits for the client to speak
		// first, so the handshake times out. Say what to do about it.
		return fmt.Errorf("the process holding %s does not speak this backend protocol (%v).\n"+
			"If it is a daemon from an older build, stop it:\n"+
			"  pkill -f 'gh monitor daemon'", socket, err)
	}
	if err := reg.Use(provider); err != nil {
		return fmt.Errorf("register the shared poller: %w", err)
	}
	return nil
}
