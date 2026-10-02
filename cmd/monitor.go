package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	cmd.Flags().IntVarP(&opts.Interval, "interval", "i", 300, "Polling interval in seconds (min 10); sets the cadence of a shared poller this command starts, not of one already running (whose cadence is its pollInterval preference or 'gh monitor daemon --interval')")
	cmd.Flags().IntVarP(&opts.Timeout, "timeout", "t", 0, "Maximum watch time in seconds (0 = run until merged/closed)")
	cmd.Flags().StringVar(&opts.IgnoredBots, "ignored-bots", "", "Comma-separated author logins whose general comments are ignored")
	cmd.Flags().StringVar(&opts.Events, "events", "", "Comma-separated list of event kinds to emit (suppresses all others); omit to emit everything")
	cmd.Flags().StringVar(&opts.Events, "only-events", "", "Alias for --events")
	cmd.Flags().StringVar(&opts.Until, "until", "", "Comma-separated event kinds; exit 0 the first time any of them fires (exit 2 if the watch ends first)")
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
	Until         string
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

	// The orphan guard (issue #129): when the process that launched this
	// watch exits, the OS reparents this one, and the changed parent pid ends
	// the watch — the same contract as Ctrl-C, exit 0. A monitor whose owner
	// is gone has nothing left to report, and a pile of such monitors shares
	// one API budget with the sessions that are still alive. Disabled watches
	// (GH_MONITOR_ORPHAN_GUARD=0) get a nil guard whose Done never fires.
	guard := maybeStartGuardFn()
	defer guard.Stop()
	go func() {
		select {
		case <-guard.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

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
		cfgDir, err := prefs.ConfigDir("")
		if err != nil {
			return fmt.Errorf("resolve config dir: %w", err)
		}
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

	// --until: a comma-separated set of event kinds using the same syntax and
	// validation as --events (ParseEventFilter rejects a typo loudly). The
	// watch ends the FIRST time any member fires; the flag is applied here at
	// the consumer loop, not in per-backend code, so it works whatever
	// transport serves the watch — the shared-poller daemon, an external
	// backend, or the in-process --once path.
	var untilFilter *monitor.EventFilter
	if strings.TrimSpace(opts.Until) != "" {
		// --timeout stays a maximum watch time, never a completion condition
		// (README, and issue #127's fix): a "timeout" member would be a
		// second way to say "end when the deadline passes", so it is
		// rejected here with its own message rather than the generic
		// unknown-kind one.
		if strings.Contains(strings.ToLower(opts.Until), "timeout") {
			return fmt.Errorf("--timeout is a maximum watch time, never a completion condition; %q is not a --until member", opts.Until)
		}
		filter, err := monitor.ParseEventFilter(opts.Until)
		if err != nil {
			return err
		}
		untilFilter = filter
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

	write := func(n monitor.Notification) error {
		var err error
		if opts.Text {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, monitor.LinkifyText(n))
			if n.Detail != "" {
				for _, line := range strings.Split(n.Detail, "\n") {
					_, _ = fmt.Fprintf(out, "  %s\n", line)
				}
			}
			return nil
		}
		if err = encodeJSON(cmd, n); err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: %v\n", err)
		}
		return err
	}

	// --events applies here, at the one boundary every notification crosses,
	// whatever produced it — the shared daemon or an external backend. A
	// failed write means the consumer of the watch's output is gone (a closed
	// pipe reports EPIPE to its writer): the watch ends there, so a monitor
	// whose reader has exited stops polling instead of streaming into the
	// void (issue #129).
	writeFailed := false
	emit := func(n monitor.Notification) {
		if writeFailed {
			return
		}
		if eventFilter == nil || eventFilter.Allows(n.Type) {
			if write(n) != nil {
				writeFailed = true
				cancel()
			}
		}
	}
	// Eyes-on-notify fires through the same boundary: a kind the filter
	// suppressed was not delivered, so nothing about it is acknowledged. ack
	// itself is built below, once the target and registry exist.
	var ack notifier
	ackEmit := func(ev backend.Event) {
		if ack != nil && (eventFilter == nil || eventFilter.Allows(string(ev.Type))) {
			ackOnDeliver(ctx, ack, ev, cmd.ErrOrStderr())
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
	// watch. A one-shot read never spawns a daemon: the built-in backend
	// answers --once with a single in-process fetch (hub.Once), so a daemon
	// per read would buy nothing. It does use a daemon that is already
	// running (issue #114). That daemon answers --once from one fetch in its
	// hub, even for a kind a sub-daemon serves, because a sub-daemon may
	// have no record of the backlog a one-shot read reports (issue #119).
	if opts.Backend.endpoint() == "" {
		if !opts.Once {
			daemonStarted, err := attachDaemon(ctx, reg, target, runOpts.Interval)
			if err != nil {
				return err
			}
			// --interval reaches the poller only when this invocation is the
			// one that started the daemon: autostart passes it down as the
			// daemon's own --interval. A daemon that was already listening
			// keeps the cadence it was built with, so the client's request is
			// dropped at hub.Subscribe, which never reads WatchOptions.Interval
			// (one poller, one cadence, shared by every subscriber). Say so,
			// once, rather than let the operator believe they got the cadence
			// they asked for. Only an explicit --interval is a request worth
			// contradicting: the default is not.
			if !daemonStarted && cmd.Flags().Changed("interval") {
				writeIntervalIgnoredNotice(cmd.ErrOrStderr(), runOpts.Interval)
			}
		} else {
			attachRunningDaemon(ctx, reg)
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

	// untilMet records whether a --until member fired before the stream
	// ended; the check after the loop turns "ended without firing" into the
	// errUntilNotMet sentinel (exit code 2).
	untilMet := false

	// The client keeps its own copy of the --timeout deadline (issue #127):
	// the daemon-side relays honour it too, but a backend whose watch
	// machinery stopped answering the deadline — the observed case is a
	// sub-daemon whose broker connection dropped and that then held the
	// stream open — leaves nothing between the dead backend and the caller.
	// The deadline is a hard one: the timer's fire ends the stream no matter
	// what state the connection is in, and the post-loop code says the watch
	// ended on the timeout, with the last degraded state attached.
	deadline := time.Time{}
	if opts.Timeout > 0 {
		deadline = time.Now().Add(time.Duration(opts.Timeout) * time.Second)
	}
	deadlineTimer := time.NewTimer(time.Hour)
	deadlineTimer.Stop()
	if opts.Timeout > 0 {
		deadlineTimer.Reset(time.Until(deadline))
	}
	defer deadlineTimer.Stop()
	timedOut := false

	// lastDegraded remembers the latest degraded update the stream
	// delivered, so the timeout line can say what state the watch was in
	// when the deadline passed (issue #127). A recovered watch (✅ notice)
	// clears it.
	var lastDegraded *backend.Update

	// Eyes-on-notify (pref reactOnNotify, default on): every comment a
	// delivered notification is about gets a 👀 reaction, so humans on the PR
	// can see the notification was received.
	if runOpts.Prefs.ReactOnNotify {
		reg2 := reg
		ack = &reactionNotifier{
			reactFn: func() (backend.ReactionActor, error) {
				actor, _, err := reg2.ReactionsFor(target)
				return actor, err
			},
			target: target,
		}
	}

	// handleUpdate runs the loop body for one delivered update: cursor
	// persist, event log, render, the --until / write-failure paths, and the
	// end-of-batch stop. It reports whether the watch should continue.
	handleUpdate := func(u backend.Update) bool {
		if !dedup.Allow(u) {
			// A dropped redelivery can still be the update that closes the
			// batch a --until member fired in.
			if untilMet && !u.More {
				return false
			}
			return true
		}
		// A named instance persists cursor state from what each update carries,
		// whatever backend delivered it — the shared daemon, an external broker
		// sub-daemon, or a one-shot read through the built-in backend. This is
		// what makes a scheduled `--once --instance` tick a DELTA against the
		// last tick instead of a full replay: the stored baseline is re-diffed
		// on the next run, so an unchanged payload emits nothing (issue: broker
		// replay flood — a fixed set of long-merged PRs re-arrived every poll
		// pass because the once path never persisted its baseline).
		if persistFromUpdate != nil {
			persistFromUpdate(u)
		}
		if evlog != nil && !evlogFailed {
			if err := evlog.Log(u); err != nil {
				evlogFailed = true
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"gh-monitor: event log write failed (%v); logging disabled for this watch\n", err)
			}
		}
		if u.Event.Type == backend.EventDegraded {
			u := u
			if u.Event.Notice != "" && strings.Contains(u.Event.Notice, "✅") {
				lastDegraded = nil
			} else {
				lastDegraded = &u
			}
		}
		n := monitor.Render(u, runOpts.Prefs, runOpts.Interval)
		// --until: the first member to fire ends the watch. It is written
		// DIRECTLY, bypassing the --events emit() suppression, so the caller
		// always learns which event triggered the exit even if that kind is
		// not in the --events allowlist. Cursor persist and the event log ran
		// above, so cursor and log stay correct on the early exit too. The
		// write failure path is the same as emit()'s: a gone consumer ends
		// the watch here as well.
		if untilFilter != nil && untilFilter.Allows(n.Type) {
			if !writeFailed {
				if write(n) != nil {
					writeFailed = true
					cancel()
				}
			}
			untilMet = true
		} else {
			emit(n)
		}
		ackEmit(u.Event)
		// The watch exits at the end of the batch the member fired in, not
		// on the member itself: the rest of that poll's batch was already
		// fetched, and on a first poll it is the PR's backlog (issue #116).
		return !untilMet || u.More
	}

	for {
		var open bool
		var u backend.Update
		if opts.Timeout > 0 {
			// A hard deadline (issue #127): the select's timer case wins over
			// a stream that stays open, so --timeout ends the watch whatever
			// state the backend connection is in.
			select {
			case <-deadlineTimer.C:
				timedOut = true
			case u, open = <-updates:
			}
		} else {
			u, open = <-updates
		}
		if !open {
			break
		}
		if !handleUpdate(u) {
			break
		}
		if timedOut {
			break
		}
	}
	ctxErr := ctx.Err()
	if ctxErr != nil && !errors.Is(ctxErr, context.Canceled) {
		return ctxErr
	}
	// A met --until condition wins over every failure ending: the member DID
	// fire and was written (or its write failed after the batch that carried
	// it), so the caller gets exit 0. A consumer that died mid-batch is a
	// loss for the next watch, not a reason to re-answer this one.
	if untilFilter != nil && untilMet {
		return nil
	}
	// The client-side deadline (issue #127): say the watch ended on the
	// timeout, with the last degraded state attached. It is emitted through
	// the plain write (not emit), so an --events allowlist cannot mute it —
	// the caller that bounded the watch must learn why it ended.
	if timedOut {
		writeTimeoutNotice(cmd, deadline, opts.Timeout, target, lastDegraded, write)
		return errUntilNotMetIfRequested(untilFilter)
	}
	// A write failure is its own ending: the consumer of the output is gone,
	// which is neither the condition being met nor the user cancelling. It
	// surfaces as an error (exit 1) so a --until caller never reads a
	// consumer-side failure as its condition having fired.
	if writeFailed {
		return errOutputConsumerGone
	}
	// The sentinel only covers a watch that ended on its own — timeout or a
	// closed stream. A Ctrl-C (context.Canceled) is the user cancelling, not
	// the condition failing, so it exits 0 as before.
	if untilFilter != nil && !untilMet && ctxErr == nil {
		return errUntilNotMet
	}
	return nil
}

// writeIntervalIgnoredNotice reports that the requested --interval did not
// reach the poller, because a shared-poller daemon that was already listening
// serves the watch at the cadence it was built with. It names the two knobs
// that do set that cadence, since the client cannot read the running daemon's
// effective one: the hello frame carries no cadence, and reporting a number
// the client guessed would be worse than naming the control.
func writeIntervalIgnoredNotice(out io.Writer, requested time.Duration) {
	_, _ = fmt.Fprintf(out,
		"gh-monitor: --interval %ds was not applied: this watch is served by a shared-poller daemon that was already running, and its poller cadence is the daemon's own (set it with the pollInterval preference or 'gh monitor daemon --interval').\n",
		int(requested.Seconds()))
}

// errUntilNotMetIfRequested maps a timed-out watch to the --until contract:
// exit 2 when the caller asked for a condition, plain success otherwise.
func errUntilNotMetIfRequested(untilFilter *monitor.EventFilter) error {
	if untilFilter != nil {
		return errUntilNotMet
	}
	return nil
}

// writeTimeoutNotice renders and writes the timeout line: the deadline that
// passed, and the last degraded state the watch was in, so a caller that
// bounded the watch knows it stopped on the clock — and can fall back to a
// REST read when the watch was degraded at the time. It goes through write()
// (never the --events filter or the until path), and a failed write is
// tolerated: the deadline was reached either way.
func writeTimeoutNotice(cmd *cobra.Command, deadline time.Time, timeoutSecs int, t backend.Target, last *backend.Update, write func(monitor.Notification) error) {
	label := t.String()
	var b strings.Builder
	fmt.Fprintf(&b, "⏰ --timeout %ds reached on %s; the watch ended on its deadline", timeoutSecs, label)
	if last != nil && last.Event.Type == backend.EventDegraded {
		detail := last.Event.Notice
		if detail == "" {
			detail = last.Event.DegradedMessage
		}
		if detail != "" {
			fmt.Fprintf(&b, " — degraded at the time: %s", detail)
		}
		if !last.At.IsZero() {
			fmt.Fprintf(&b, " (since %s)", last.At.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "; read the target over REST if you need its current state")
	}
	_ = write(monitor.Notification{
		Type:      "timeout",
		PRLabel:   label,
		Message:   b.String(),
		Timestamp: deadline,
	})
	_ = cmd // cmd is unused today; the writer carries the output stream
}

// errUntilNotMet is the sentinel runMonitor returns when a --until watch ends
// without any member of the set having fired (e.g. the --timeout safeguard
// expired or the stream closed). ExecuteOrExit maps it to exit code 2 so a
// caller can tell "condition met" (0) from "gave up" (2) without parsing
// output; a Ctrl-C (context.Canceled) is neither and exits 0 as before.
var errUntilNotMet = errors.New("--until condition not met before the watch ended")

// errOutputConsumerGone is the sentinel runMonitor returns when a write to
// the watch's output failed (a closed pipe reports EPIPE to its writer). The
// consumer is gone; the watch ends and reports an error (exit 1) rather than
// success, so a --until caller never mistakes its own death for the
// condition having fired (issue #129).
var errOutputConsumerGone = errors.New("output consumer is gone; the watch ended")

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

	// The orphan guard, the same as a target watch: a repo-wide readiness
	// watch is equally resident and equally wasteful once its owner is gone.
	guard := maybeStartGuardFn()
	defer guard.Stop()
	go func() {
		select {
		case <-guard.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

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

	// A failed write means the consumer of the readiness output is gone; the
	// cancelled context ends the watch at the loop's next check (issue #129).
	emit := func(n monitor.Notification) {
		if opts.Text {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, n.Message)
			return
		}
		if err := encodeJSON(cmd, n); err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: %v\n", err)
			cancel()
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
func attachDaemon(ctx context.Context, reg *backend.Registry, target backend.Target, interval time.Duration) (started bool, err error) {
	socket := daemonSocketPath()

	if probe, err := ipc.Dial(socket); err == nil {
		// Only a liveness check — leaving it open would strand a server
		// goroutine on a request that never comes.
		_ = probe.Close()
		// A daemon that was already listening keeps the cadence it was built
		// with; the caller's interval never reaches it.
	} else {
		if !daemonAutostart() {
			return false, fmt.Errorf("no shared poller is listening on %s and autostart is disabled (GH_MONITOR_AUTOSTART=0); start one with 'gh monitor daemon'", socket)
		}
		if err := autostartDaemon(ctx, socket, interval); err != nil {
			return false, fmt.Errorf("could not start the shared poller (%v); start one with 'gh monitor daemon'", err)
		}
		// The daemon this watch is about to use was started from this
		// invocation's own interval, so the request was honoured.
		started = true
	}

	transport, err := remote.ParseEndpoint("unix:" + socket)
	if err != nil {
		return false, fmt.Errorf("parse daemon endpoint: %w", err)
	}
	provider, err := remote.Connect(ctx, transport)
	if err != nil {
		// The likeliest cause is a daemon left running from a build before
		// this protocol: it holds the socket and waits for the client to speak
		// first, so the handshake times out. Say what to do about it.
		return false, fmt.Errorf("the process holding %s does not speak this backend protocol (%v).\n"+
			"If it is a daemon from an older build, stop it:\n"+
			"  pkill -f 'gh monitor daemon'", socket, err)
	}
	if err := reg.Use(provider); err != nil {
		return false, fmt.Errorf("register the shared poller: %w", err)
	}
	return started, nil
}

// attachRunningDaemon registers the shared poller for a --once read when a
// daemon is already listening, and does nothing otherwise. It never spawns a
// daemon, and a daemon it cannot talk to is not an error: the built-in
// backend answers the read in-process instead.
func attachRunningDaemon(ctx context.Context, reg *backend.Registry) {
	socket := daemonSocketPath()
	probe, err := ipc.Dial(socket)
	if err != nil {
		return
	}
	_ = probe.Close()
	transport, err := remote.ParseEndpoint("unix:" + socket)
	if err != nil {
		return
	}
	provider, err := remote.Connect(ctx, transport)
	if err != nil {
		return
	}
	_ = reg.Use(provider)
}
