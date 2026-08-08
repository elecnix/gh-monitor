package monitor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// firstPollType and allClearType are loop-level notification kinds that are not
// Diff events but do have templates in prefs.
const (
	firstPollType = "first-poll"
)

// maxIdleInterval caps the adaptive idle backoff, and maxErrBackoff caps the
// transient-error backoff. Both mirror pi-ghpr-monitor's 5-minute ceilings.
const (
	maxIdleInterval = 300 * time.Second
	maxErrBackoff   = 300 * time.Second
	defaultInterval = 60 * time.Second
)

// maxFailedLogLines caps the failed-run log snippet embedded in a
// run-completed notification (issue #19). `gh run view --log-failed` emits the
// full failed-step log chronologically, with the actual error at the END, so we
// keep the last N lines (the error + its immediate context) rather than the
// first N — taking the head would capture only setup noise and miss the error.
const maxFailedLogLines = 50

// runFailureConclusions are the terminal conclusions that mean the run did not
// succeed and therefore warrant a failed-log snippet.
var runFailureConclusions = map[string]bool{
	"failure": true, "timed_out": true, "cancelled": true, "action_required": true,
}

// isRunFailureConclusion reports whether a run conclusion is a failure variant.
func isRunFailureConclusion(c string) bool { return runFailureConclusions[c] }

var (
	// ansiRE strips ANSI/VT100 escape sequences (color codes, cursor moves,
	// etc.) that `gh run view --log-failed` embeds. It matches both a real ESC
	// byte (\x1b) and the caret-notation form (`^[[...m`) that gh emits when its
	// stdout is not a TTY (e.g. when invoked from exec.Command).
	ansiRE = regexp.MustCompile(`(?:\x1b|\^)\[\[?[0-9;?]*[a-zA-Z]`)
	// logTsRE strips the per-line leading timestamp (`<RFC3339Nano>Z `) that
	// `gh run view --log-failed` prefixes each log line with. An optional leading
	// BOM is tolerated for the first line.
	logTsRE = regexp.MustCompile(`^\x{feff}?\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z\s?`)
)

// cleanFailedLogLine strips the per-line timestamp and ANSI color codes that
// `gh run view --log-failed` embeds, collapsing the `workflow\tjob\t<ts> <body>`
// format to `job\t<body>`. The workflow name (field 0) is dropped as redundant
// (it is the run being watched); the job name is kept so a consumer can tell
// which job failed. Lines that do not match the format pass through with ANSI
// stripped only.
func cleanFailedLogLine(line string) string {
	line = ansiRE.ReplaceAllString(line, "")
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) < 3 {
		return line
	}
	return fields[1] + "\t" + logTsRE.ReplaceAllString(fields[2], "")
}

// summarizeFailedLog cleans and tail-truncates the failed-run log output so the
// snippet carries the error (which lives at the end of the `--log-failed`
// output) plus its immediate context. When the cleaned log fits within maxLines
// it is returned verbatim; otherwise the last maxLines lines are kept, prefixed
// by a one-line marker noting how many earlier lines were dropped. Empty input
// yields "".
func summarizeFailedLog(s string, maxLines int) string {
	s = strings.TrimPrefix(s, "\ufeff") // strip a leading BOM if present
	if strings.TrimSpace(s) == "" {
		return ""
	}
	raw := strings.Split(s, "\n")
	if len(raw) > 0 && raw[len(raw)-1] == "" { // drop trailing empty line from final "\n"
		raw = raw[:len(raw)-1]
	}
	cleaned := make([]string, len(raw))
	for i, l := range raw {
		cleaned[i] = cleanFailedLogLine(l)
	}
	if len(cleaned) <= maxLines {
		return strings.Join(cleaned, "\n")
	}
	dropped := len(cleaned) - maxLines
	return fmt.Sprintf("… (%d earlier lines truncated)\n%s", dropped, strings.Join(cleaned[len(cleaned)-maxLines:], "\n"))
}

// failedRunLogDetail fetches and summarizes the failed-run log snippet for a
// completed run. A fetch error is logged to stderr and yields an empty detail
// (the run-completed notification still emits).
func failedRunLogDetail(svc *Service, id resolver.Identity, runID int) string {
	out, err := svc.FailedRunLogs(id.Owner, id.Repo, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-monitor: failed-run log fetch error: %v\n", err)
		return ""
	}
	return summarizeFailedLog(out, maxFailedLogLines)
}

// Notification is one emitted event, rendered for a consumer. It serializes to a
// single NDJSON line; a persistent watcher (e.g. Claude Code's Monitor tool)
// surfaces each line as a session notification.
type Notification struct {
	Type              string   `json:"type"`
	PRLabel           string   `json:"pr_label"`
	Message           string   `json:"message"`
	UnresolvedThreads int      `json:"unresolved_threads,omitempty"`
	GeneralComments   int      `json:"general_comments,omitempty"`
	FailingChecks     []string `json:"failing_checks,omitempty"`
	CommitShortOid    string   `json:"commit_short_oid,omitempty"`
	CommitAuthor      string   `json:"commit_author,omitempty"`
	ReviewAuthor      string   `json:"review_author,omitempty"`
	// Detail is a rich, self-contained body (thread/comment excerpts + hints) so
	// a consumer can act without extra API calls (new-*-threads/comments only).
	Detail string `json:"detail,omitempty"`
	// PRUrl and CommitUrl back the OSC-8 links in --text output.
	PRUrl     string `json:"pr_url,omitempty"`
	CommitUrl string `json:"commit_url,omitempty"`
	// RunID and Conclusion are set for workflow-run events (run-* types).
	RunID      int       `json:"run_id,omitempty"`
	Conclusion string    `json:"conclusion,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// RunOptions configures a monitor run.
type RunOptions struct {
	Identity resolver.Identity
	Prefs    prefs.Preferences
	Interval time.Duration
	// Timeout stops the loop after this duration; 0 means run forever.
	Timeout time.Duration

	// EventFilter is a per-event-kind allowlist. A non-nil filter suppresses
	// (drops) any Notification whose Type is not in the allowlist before it
	// reaches the caller's emit callback; a nil filter (the default) emits
	// every kind, preserving today's behaviour. Use NewEventFilter to build
	// one programmatically or ParseEventFilter to validate caller input.
	EventFilter *EventFilter

	// AnnotationLevels controls which check-run annotation levels are
	// included in the snapshot. A nil value uses the default (warning +
	// failure). An empty filter ("none") drops all annotations. Use
	// NewAnnotationLevels to build one programmatically or
	// ParseAnnotationLevels to validate caller input.
	AnnotationLevels *AnnotationLevels

	// Now and Sleep are injectable for tests. Now defaults to time.Now; Sleep
	// defaults to a context-aware timer. Sleep must return the context error
	// when the context is cancelled.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

func (o *RunOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *RunOptions) sleep(ctx context.Context, d time.Duration) error {
	if o.Sleep != nil {
		return o.Sleep(ctx, d)
	}
	return realSleep(ctx, d)
}

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run polls the target until it reaches a terminal state, the context is
// cancelled, or the timeout elapses, emitting one Notification per
// genuinely-new change. Dispatches based on the target type in opts.Identity.
//
// If opts.EventFilter is non-nil, notifications whose Type is not in the
// allowlist are dropped before they reach emit (per-event-kind filtering for
// orchestrator/automation callers that only care about a subset of events).
func Run(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	emit = filterEmit(opts.EventFilter, emit)
	switch opts.Identity.Target {
	case "ref", "commit":
		return runRef(ctx, svc, opts, emit)
	case "issue":
		return runIssue(ctx, svc, opts, emit)
	case "run":
		return runRun(ctx, svc, opts, emit)
	case "repo":
		return runRepo(ctx, svc, opts, emit)
	default:
		return runPR(ctx, svc, opts, emit)
	}
}

// Once does a single fetch and emits the current actionable state. If
// opts.EventFilter is non-nil, notifications whose Type is not in the
// allowlist are dropped before they reach emit.
func Once(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	emit = filterEmit(opts.EventFilter, emit)
	switch opts.Identity.Target {
	case "ref", "commit":
		return onceRef(ctx, svc, opts, emit)
	case "issue":
		return onceIssue(ctx, svc, opts, emit)
	case "run":
		return onceRun(ctx, svc, opts, emit)
	case "repo":
		return onceRepo(ctx, svc, opts, emit)
	default:
		return oncePR(ctx, svc, opts, emit)
	}
}

// ---------------------------------------------------------------------------
// PR target
// ---------------------------------------------------------------------------

// runPR polls the PR until it is merged/closed, the context is cancelled, or
// the timeout elapses.
func runPR(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	base := opts.Interval
	if base <= 0 {
		base = defaultInterval
	}

	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = opts.now().Add(opts.Timeout)
	}

	c := NewPRConsumer(opts)
	errBackoff := time.Duration(0)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		resp, err := svc.Fetch(&opts.Identity, opts.Identity.Number)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: fetch error: %v\n", err)
			errBackoff = nextErrBackoff(errBackoff, base)
			if serr := opts.sleep(ctx, errBackoff); serr != nil {
				return serr
			}
			continue
		}
		errBackoff = 0

		curr := Snapshot(resp.Repository.PullRequest, SnapshotOptions{IgnoredBots: opts.Prefs.IgnoredBots, AnnotationLevels: opts.AnnotationLevels})
		if c.Consume(curr, emit) {
			return nil
		}

		d := idleInterval(base, c.noChange)
		if !deadline.IsZero() {
			remaining := deadline.Sub(opts.now())
			if remaining <= 0 {
				return nil
			}
			if d > remaining {
				d = remaining
			}
		}
		if err := opts.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func oncePR(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	resp, err := svc.Fetch(&opts.Identity, opts.Identity.Number)
	if err != nil {
		return err
	}
	curr := Snapshot(resp.Repository.PullRequest, SnapshotOptions{IgnoredBots: opts.Prefs.IgnoredBots, AnnotationLevels: opts.AnnotationLevels})
	emit(renderNotificationPR(opts, curr, firstPollType, Event{}))
	for _, ev := range Diff(&PRStatus{}, curr) {
		if ev.Type == EventNewCommit {
			continue // agent just pushed it
		}
		emit(renderNotificationPR(opts, curr, string(ev.Type), ev))
	}
	// ci-all-green never fires against an empty baseline — emit it explicitly.
	if ciAllGreen(curr) {
		emit(renderNotificationPR(opts, curr, string(EventCIAllGreen), Event{Type: EventCIAllGreen}))
	}
	return nil
}

// ciAllGreen reports whether an open PR's CI has finished with every check
// passing. It requires at least one successful check: with no checks at all —
// the state GitHub reports for the first seconds after a push, and the
// permanent state of a repo without CI — FailingChecks and PendingChecks are
// empty too, and announcing green there would be a guess, not an observation.
func ciAllGreen(s *PRStatus) bool {
	return !s.Merged && s.State != "CLOSED" &&
		len(s.FailingChecks) == 0 && len(s.PendingChecks) == 0 &&
		len(s.SuccessfulChecks) > 0
}

// ---------------------------------------------------------------------------
// Ref / commit target
// ---------------------------------------------------------------------------

// runRef polls a ref or commit, emitting CI events until the context is
// cancelled or timeout elapses. Ref targets never auto-stop (no terminal
// state), so they run until cancelled or timed out.
func runRef(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	base := opts.Interval
	if base <= 0 {
		base = defaultInterval
	}

	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = opts.now().Add(opts.Timeout)
	}

	var prev *RefStatus
	noChange := 0
	errBackoff := time.Duration(0)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var curr *RefStatus
		switch opts.Identity.Target {
		case "commit":
			resp, err := svc.FetchCommit(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.CommitSHA)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gh-monitor: fetch error: %v\n", err)
				errBackoff = nextErrBackoff(errBackoff, base)
				if serr := opts.sleep(ctx, errBackoff); serr != nil {
					return serr
				}
				continue
			}
			curr = SnapshotCommit(resp.Repository.Object)
		default:
			resp, err := svc.FetchRef(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.Ref)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gh-monitor: fetch error: %v\n", err)
				errBackoff = nextErrBackoff(errBackoff, base)
				if serr := opts.sleep(ctx, errBackoff); serr != nil {
					return serr
				}
				continue
			}
			curr = SnapshotRef(resp.Repository.Ref)
		}
		errBackoff = 0

		firstPoll := prev == nil
		// On the first poll, diff against an empty baseline so all pre-existing
		// CI issues are surfaced immediately.
		compare := prev
		if firstPoll {
			compare = &RefStatus{}
			emit(renderNotificationRef(opts, curr, firstPollType, Event{}))
		}
		events := DiffRef(compare, curr)
		for _, ev := range events {
			emit(renderNotificationRef(opts, curr, string(ev.Type), ev))
		}
		if len(events) == 0 {
			noChange++
		} else {
			noChange = 0
		}
		prev = curr

		d := idleInterval(base, noChange)
		if !deadline.IsZero() {
			remaining := deadline.Sub(opts.now())
			if remaining <= 0 {
				return nil
			}
			if d > remaining {
				d = remaining
			}
		}
		if err := opts.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func onceRef(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	var curr *RefStatus
	switch opts.Identity.Target {
	case "commit":
		resp, err := svc.FetchCommit(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.CommitSHA)
		if err != nil {
			return err
		}
		curr = SnapshotCommit(resp.Repository.Object)
	default:
		resp, err := svc.FetchRef(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.Ref)
		if err != nil {
			return err
		}
		curr = SnapshotRef(resp.Repository.Ref)
	}
	emit(renderNotificationRef(opts, curr, firstPollType, Event{}))
	for _, ev := range DiffRef(&RefStatus{}, curr) {
		emit(renderNotificationRef(opts, curr, string(ev.Type), ev))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Issue target
// ---------------------------------------------------------------------------

// runIssue polls an issue, emitting state-change and comment events. Auto-stops
// when the issue is closed (but not reopened — a reopened issue continues).
func runIssue(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	base := opts.Interval
	if base <= 0 {
		base = defaultInterval
	}

	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = opts.now().Add(opts.Timeout)
	}

	var prev *IssueStatus
	noChange := 0
	errBackoff := time.Duration(0)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		resp, err := svc.FetchIssue(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.Number)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: fetch error: %v\n", err)
			errBackoff = nextErrBackoff(errBackoff, base)
			if serr := opts.sleep(ctx, errBackoff); serr != nil {
				return serr
			}
			continue
		}
		errBackoff = 0

		curr := SnapshotIssue(resp.Repository.Issue, SnapshotOptions{IgnoredBots: opts.Prefs.IgnoredBots})

		firstPoll := prev == nil
		terminalEmitted := false
		// On the first poll, diff against an empty baseline so all pre-existing
		// comments are surfaced immediately.
		compare := prev
		if firstPoll {
			compare = &IssueStatus{}
			emit(renderNotificationIssue(opts, curr, firstPollType, Event{}))
		}
		events := DiffIssues(compare, curr)
		for _, ev := range events {
			emit(renderNotificationIssue(opts, curr, string(ev.Type), ev))
			if ev.Type == EventIssueClosed {
				terminalEmitted = true
			}
		}
		if len(events) == 0 {
			noChange++
		} else {
			noChange = 0
		}
		prev = curr

		if curr.State == "CLOSED" {
			if firstPoll && !terminalEmitted {
				emit(renderNotificationIssue(opts, curr, string(EventIssueClosed), Event{Type: EventIssueClosed}))
			}
			return nil
		}

		d := idleInterval(base, noChange)
		if !deadline.IsZero() {
			remaining := deadline.Sub(opts.now())
			if remaining <= 0 {
				return nil
			}
			if d > remaining {
				d = remaining
			}
		}
		if err := opts.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func onceIssue(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	resp, err := svc.FetchIssue(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.Number)
	if err != nil {
		return err
	}
	curr := SnapshotIssue(resp.Repository.Issue, SnapshotOptions{IgnoredBots: opts.Prefs.IgnoredBots})
	emit(renderNotificationIssue(opts, curr, firstPollType, Event{}))
	for _, ev := range DiffIssues(&IssueStatus{}, curr) {
		emit(renderNotificationIssue(opts, curr, string(ev.Type), ev))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// idleInterval returns the poll interval given the number of consecutive
// no-change polls: base until 3 no-change polls, then base*2^(n-3) capped at
// maxIdleInterval.
func idleInterval(base time.Duration, noChange int) time.Duration {
	d := base
	if noChange >= 3 {
		shift := uint(noChange - 3)
		if shift > 20 {
			shift = 20
		}
		d = base * time.Duration(uint64(1)<<shift)
	}
	if d > maxIdleInterval {
		d = maxIdleInterval
	}
	if d < base {
		d = base
	}
	return d
}

// nextErrBackoff doubles the current error backoff (starting at base), capped.
func nextErrBackoff(cur, base time.Duration) time.Duration {
	if cur <= 0 {
		if base > maxErrBackoff {
			return maxErrBackoff
		}
		return base
	}
	d := cur * 2
	if d > maxErrBackoff {
		d = maxErrBackoff
	}
	return d
}

// ---------------------------------------------------------------------------
// PR notification rendering
// ---------------------------------------------------------------------------

// renderNotificationPR builds the interpolation vars, renders the template for
// typ, and populates the structured fields relevant to the event.
func renderNotificationPR(opts RunOptions, status *PRStatus, typ string, ev Event) Notification {
	vars := buildVarsPR(opts.Identity, status, ev, opts.Interval)
	n := Notification{
		Type:      typ,
		PRLabel:   vars["prLabel"],
		Message:   prefs.Interpolate(opts.Prefs.Templates[typ], vars),
		Timestamp: opts.now(),
	}
	n.PRUrl = vars["prUrl"]
	if status != nil {
		n.UnresolvedThreads = len(status.UnresolvedThreads)
		n.GeneralComments = len(status.GeneralComments)
	}
	switch EventType(typ) {
	case EventNewFailingChecks:
		if len(ev.Checks) > 0 {
			n.FailingChecks = ev.Checks
		} else if status != nil {
			n.FailingChecks = status.FailingChecks
		}
		// When the PR has merge conflicts, CI failures are often caused by
		// the conflict rather than an Actions outage. Guide the agent to
		// resolve the conflict first.
		if status != nil && status.Conflict {
			n.Detail = "The PR has merge conflicts — they may be causing these CI failures. Resolve the conflict first."
		}
	case EventNewUnresolvedThreads:
		n.Detail = threadsDetail(ev.Threads)
	case EventNewGeneralComments:
		n.Detail = commentsDetail(ev.Comments)
	case EventNewCommit:
		if ev.Commit != nil {
			n.CommitShortOid = ev.Commit.ShortOid
			n.CommitAuthor = ev.Commit.Author
			n.CommitUrl = vars["commitUrl"]
		}
	case EventReviewApproved, EventReviewChangesRequested, EventReviewDismissed:
		n.ReviewAuthor = ev.ReviewAuthor
	case EventCheckAnnotations:
		n.Detail = annotationsDetail(ev.Annotations)
	}
	return n
}

func buildVarsPR(id resolver.Identity, status *PRStatus, ev Event, interval time.Duration) map[string]string {
	host := id.Host
	if host == "" {
		host = "github.com"
	}
	vars := map[string]string{
		"owner":       id.Owner,
		"repo":        id.Repo,
		"number":      strconv.Itoa(id.Number),
		"host":        host,
		"prLabel":     fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, id.Number),
		"prUrl":       fmt.Sprintf("https://%s/%s/%s/pull/%d", host, id.Owner, id.Repo, id.Number),
		"intervalSec": strconv.Itoa(int(interval.Seconds())),
	}

	if status != nil {
		vars["unresolvedThreads"] = strconv.Itoa(len(status.UnresolvedThreads))
		vars["generalComments"] = strconv.Itoa(len(status.GeneralComments))
		vars["failingChecks"] = strings.Join(status.FailingChecks, ", ")
		vars["conflict"] = strconv.FormatBool(status.Conflict)
		vars["reviewAuthor"] = status.ReviewAuthor
		setCommitVars(vars, host, id, status.LastCommit)
	}

	if ev.Type == EventNewFailingChecks && len(ev.Checks) > 0 {
		vars["failingChecks"] = strings.Join(ev.Checks, ", ")
	}
	if ev.ReviewAuthor != "" {
		vars["reviewAuthor"] = ev.ReviewAuthor
	}
	if ev.Commit != nil {
		setCommitVars(vars, host, id, *ev.Commit)
	}

	// Annotation vars for check-annotations events.
	if len(ev.Annotations) > 0 {
		vars["annotationCount"] = strconv.Itoa(len(ev.Annotations))
		names := make([]string, 0, len(ev.Annotations))
		seen := map[string]bool{}
		for _, a := range ev.Annotations {
			if !seen[a.CheckName] {
				seen[a.CheckName] = true
				names = append(names, a.CheckName)
			}
		}
		vars["annotationCheckNames"] = strings.Join(names, ", ")
	}

	return vars
}

// ---------------------------------------------------------------------------
// Ref notification rendering
// ---------------------------------------------------------------------------

func renderNotificationRef(opts RunOptions, status *RefStatus, typ string, ev Event) Notification {
	vars := buildVarsRef(opts.Identity, status, ev, opts.Interval)
	n := Notification{
		Type:      typ,
		PRLabel:   vars["prLabel"],
		Message:   prefs.Interpolate(opts.Prefs.Templates[typ], vars),
		Timestamp: opts.now(),
	}
	n.PRUrl = vars["prUrl"]
	switch EventType(typ) {
	case EventNewFailingChecks:
		if len(ev.Checks) > 0 {
			n.FailingChecks = ev.Checks
		} else if status != nil {
			n.FailingChecks = status.FailingChecks
		}
	case EventNewCommit:
		if ev.Commit != nil {
			n.CommitShortOid = ev.Commit.ShortOid
			n.CommitAuthor = ev.Commit.Author
			n.CommitUrl = vars["commitUrl"]
		}
	}
	return n
}

func buildVarsRef(id resolver.Identity, status *RefStatus, ev Event, interval time.Duration) map[string]string {
	host := id.Host
	if host == "" {
		host = "github.com"
	}

	label := fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.Ref)
	refURL := fmt.Sprintf("https://%s/%s/%s/tree/%s", host, id.Owner, id.Repo, id.Ref)
	if id.Target == "commit" {
		label = fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.CommitSHA)
		if len(id.CommitSHA) > 7 {
			label = fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.CommitSHA[:7])
		}
		refURL = fmt.Sprintf("https://%s/%s/%s/commit/%s", host, id.Owner, id.Repo, id.CommitSHA)
	}

	vars := map[string]string{
		"owner":       id.Owner,
		"repo":        id.Repo,
		"number":      "0", // ref targets don't have a number
		"host":        host,
		"prLabel":     label,
		"prUrl":       refURL,
		"intervalSec": strconv.Itoa(int(interval.Seconds())),
	}

	if status != nil {
		vars["failingChecks"] = strings.Join(status.FailingChecks, ", ")
		cs := CommitSummary{
			Oid:             status.Oid,
			ShortOid:        status.ShortOid,
			Author:          status.Author,
			MessageHeadline: status.MessageHeadline,
		}
		setCommitVars(vars, host, id, cs)
	}

	if ev.Type == EventNewFailingChecks && len(ev.Checks) > 0 {
		vars["failingChecks"] = strings.Join(ev.Checks, ", ")
	}
	if ev.Commit != nil {
		setCommitVars(vars, host, id, *ev.Commit)
	}

	return vars
}

// ---------------------------------------------------------------------------
// Issue notification rendering
// ---------------------------------------------------------------------------

func renderNotificationIssue(opts RunOptions, status *IssueStatus, typ string, ev Event) Notification {
	vars := buildVarsIssue(opts.Identity, status, ev, opts.Interval)
	n := Notification{
		Type:      typ,
		PRLabel:   vars["prLabel"],
		Message:   prefs.Interpolate(opts.Prefs.Templates[typ], vars),
		Timestamp: opts.now(),
	}
	n.PRUrl = vars["prUrl"]
	switch EventType(typ) {
	case EventIssueNewComment, EventIssueMention:
		if len(ev.IssueComments) > 0 {
			n.Detail = issueCommentsDetail(ev.IssueComments)
		}
	}
	return n
}

func buildVarsIssue(id resolver.Identity, status *IssueStatus, ev Event, interval time.Duration) map[string]string {
	host := id.Host
	if host == "" {
		host = "github.com"
	}
	vars := map[string]string{
		"owner":       id.Owner,
		"repo":        id.Repo,
		"number":      strconv.Itoa(id.Number),
		"host":        host,
		"prLabel":     fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, id.Number),
		"prUrl":       fmt.Sprintf("https://%s/%s/%s/issues/%d", host, id.Owner, id.Repo, id.Number),
		"intervalSec": strconv.Itoa(int(interval.Seconds())),
	}

	if status != nil {
		vars["issueState"] = status.State
		vars["issueTitle"] = status.Title
		vars["issueComments"] = strconv.Itoa(len(status.Comments))
	}

	return vars
}

// issueCommentsDetail joins the per-comment details for issue comments.
func issueCommentsDetail(comments []IssueCommentSummary) string {
	parts := make([]string, 0, len(comments))
	for _, c := range comments {
		parts = append(parts, fmt.Sprintf("%s: %s", c.Author, c.Body))
	}
	return strings.Join(parts, "\n\n")
}

// ---------------------------------------------------------------------------
// Shared commit vars
// ---------------------------------------------------------------------------

func setCommitVars(vars map[string]string, host string, id resolver.Identity, c CommitSummary) {
	vars["commitOid"] = c.Oid
	vars["commitShortOid"] = c.ShortOid
	vars["commitAuthor"] = c.Author
	vars["commitCoauthors"] = strings.Join(c.Coauthors, ", ")
	vars["commitMessageHeadline"] = c.MessageHeadline
	if c.Oid != "" {
		vars["commitUrl"] = fmt.Sprintf("https://%s/%s/%s/commit/%s", host, id.Owner, id.Repo, c.Oid)
	}
}

// ---------------------------------------------------------------------------
// Workflow-run target
// ---------------------------------------------------------------------------

// runRun polls a single GitHub Actions workflow run until it reaches a terminal
// conclusion (status == "completed"), the context is cancelled, or the timeout
// elapses. Unlike ref/commit monitoring (which never auto-stops), a run has a
// clear terminal state and the loop stops once it is reached — mirroring the PR
// loop's merged/closed exit.
func runRun(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	base := opts.Interval
	if base <= 0 {
		base = defaultInterval
	}

	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = opts.now().Add(opts.Timeout)
	}

	var prev *RunStatus
	noChange := 0
	errBackoff := time.Duration(0)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		resp, err := svc.FetchRun(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.RunID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: fetch error: %v\n", err)
			errBackoff = nextErrBackoff(errBackoff, base)
			if serr := opts.sleep(ctx, errBackoff); serr != nil {
				return serr
			}
			continue
		}
		errBackoff = 0

		curr := SnapshotRun(resp)

		firstPoll := prev == nil
		terminalEmitted := false
		// On the first poll, diff against an empty baseline so any pre-existing
		// state (e.g. a run that is already completed) is surfaced immediately.
		compare := prev
		if firstPoll {
			compare = &RunStatus{}
			emit(renderNotificationRun(opts, curr, firstPollType, Event{}))
		}
		events := DiffRun(compare, curr)
		for _, ev := range events {
			n := renderNotificationRun(opts, curr, string(ev.Type), ev)
			if ev.Type == EventRunCompleted && isRunFailureConclusion(ev.RunConclusion) {
				n.Detail = failedRunLogDetail(svc, opts.Identity, curr.RunID)
			}
			emit(n)
			if ev.Type == EventRunCompleted {
				terminalEmitted = true
			}
		}
		if len(events) == 0 {
			noChange++
		} else {
			noChange = 0
		}
		prev = curr

		if curr.IsTerminal() {
			if firstPoll && !terminalEmitted {
				ev := Event{Type: EventRunCompleted, RunConclusion: curr.Conclusion}
				n := renderNotificationRun(opts, curr, string(EventRunCompleted), ev)
				if isRunFailureConclusion(curr.Conclusion) {
					n.Detail = failedRunLogDetail(svc, opts.Identity, curr.RunID)
				}
				emit(n)
			}
			return nil
		}

		d := idleInterval(base, noChange)
		if !deadline.IsZero() {
			remaining := deadline.Sub(opts.now())
			if remaining <= 0 {
				return nil
			}
			if d > remaining {
				d = remaining
			}
		}
		if err := opts.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func onceRun(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	resp, err := svc.FetchRun(opts.Identity.Owner, opts.Identity.Repo, opts.Identity.RunID)
	if err != nil {
		return err
	}
	curr := SnapshotRun(resp)
	emit(renderNotificationRun(opts, curr, firstPollType, Event{}))
	for _, ev := range DiffRun(&RunStatus{}, curr) {
		n := renderNotificationRun(opts, curr, string(ev.Type), ev)
		if ev.Type == EventRunCompleted && isRunFailureConclusion(ev.RunConclusion) {
			n.Detail = failedRunLogDetail(svc, opts.Identity, curr.RunID)
		}
		emit(n)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Workflow-run notification rendering
// ---------------------------------------------------------------------------

// renderNotificationRun builds the interpolation vars, renders the template
// for typ, and populates the structured fields relevant to the run event.
func renderNotificationRun(opts RunOptions, status *RunStatus, typ string, ev Event) Notification {
	vars := buildVarsRun(opts.Identity, status, ev, opts.Interval)
	n := Notification{
		Type:      typ,
		PRLabel:   vars["prLabel"],
		Message:   prefs.Interpolate(opts.Prefs.Templates[typ], vars),
		Timestamp: opts.now(),
		PRUrl:     vars["prUrl"],
	}
	if status != nil {
		n.RunID = status.RunID
		if ev.Type == EventRunCompleted {
			n.Conclusion = ev.RunConclusion
		}
		if status.ShortSHA != "" {
			n.CommitShortOid = status.ShortSHA
			n.CommitUrl = vars["commitUrl"]
		}
	}
	return n
}

func buildVarsRun(id resolver.Identity, status *RunStatus, ev Event, interval time.Duration) map[string]string {
	host := id.Host
	if host == "" {
		host = "github.com"
	}
	vars := map[string]string{
		"owner":       id.Owner,
		"repo":        id.Repo,
		"host":        host,
		"intervalSec": strconv.Itoa(int(interval.Seconds())),
	}

	if status != nil {
		vars["runId"] = strconv.Itoa(status.RunID)
		vars["runName"] = status.Name
		vars["runNumber"] = strconv.Itoa(status.RunNumber)
		vars["runEvent"] = status.Event
		vars["runStatus"] = status.Status
		vars["runConclusion"] = status.Conclusion
		vars["runBranch"] = status.HeadBranch
		vars["runUrl"] = status.HTMLURL
		vars["prLabel"] = fmt.Sprintf("%s/%s run #%d", id.Owner, id.Repo, status.RunNumber)
		vars["prUrl"] = status.HTMLURL
		if status.HeadSHA != "" {
			vars["commitOid"] = status.HeadSHA
			vars["commitShortOid"] = status.ShortSHA
			vars["commitUrl"] = fmt.Sprintf("https://%s/%s/%s/commit/%s", host, id.Owner, id.Repo, status.HeadSHA)
		}
	} else {
		vars["prLabel"] = fmt.Sprintf("%s/%s run #%d", id.Owner, id.Repo, id.RunID)
		vars["prUrl"] = fmt.Sprintf("https://%s/%s/%s/actions/runs/%d", host, id.Owner, id.Repo, id.RunID)
	}

	if ev.Type == EventRunCompleted {
		vars["runConclusion"] = ev.RunConclusion
	}

	return vars
}

// ---------------------------------------------------------------------------
// Repo target (watch a repository for new PRs and issues)
// ---------------------------------------------------------------------------

// runRepo polls a repository for new PRs and issues, emitting events for each
// new item. Unlike PR/issue targets, repo targets never auto-stop — they run
// until cancelled or timed out.
func runRepo(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	base := opts.Interval
	if base <= 0 {
		base = defaultInterval
	}

	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = opts.now().Add(opts.Timeout)
	}

	var prev *RepoStatus
	noChange := 0
	errBackoff := time.Duration(0)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		resp, err := svc.FetchRepo(opts.Identity.Owner, opts.Identity.Repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: fetch error: %v\n", err)
			errBackoff = nextErrBackoff(errBackoff, base)
			if serr := opts.sleep(ctx, errBackoff); serr != nil {
				return serr
			}
			continue
		}
		errBackoff = 0

		curr := SnapshotRepo(resp)

		firstPoll := prev == nil
		// On the first poll, diff against an empty baseline so all pre-existing
		// PRs and issues are surfaced immediately.
		compare := prev
		if firstPoll {
			compare = &RepoStatus{}
			emit(renderNotificationRepo(opts, curr, firstPollType, Event{}))
		}
		events := DiffRepo(compare, curr)
		for _, ev := range events {
			emit(renderNotificationRepo(opts, curr, string(ev.Type), ev))
		}
		if len(events) == 0 {
			noChange++
		} else {
			noChange = 0
		}
		prev = curr

		d := idleInterval(base, noChange)
		if !deadline.IsZero() {
			remaining := deadline.Sub(opts.now())
			if remaining <= 0 {
				return nil
			}
			if d > remaining {
				d = remaining
			}
		}
		if err := opts.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func onceRepo(ctx context.Context, svc *Service, opts RunOptions, emit func(Notification)) error {
	resp, err := svc.FetchRepo(opts.Identity.Owner, opts.Identity.Repo)
	if err != nil {
		return err
	}
	curr := SnapshotRepo(resp)
	emit(renderNotificationRepo(opts, curr, firstPollType, Event{}))
	for _, ev := range DiffRepo(&RepoStatus{}, curr) {
		emit(renderNotificationRepo(opts, curr, string(ev.Type), ev))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Repo notification rendering
// ---------------------------------------------------------------------------

func renderNotificationRepo(opts RunOptions, status *RepoStatus, typ string, ev Event) Notification {
	vars := buildVarsRepo(opts.Identity, status, ev, opts.Interval)
	n := Notification{
		Type:      typ,
		PRLabel:   vars["prLabel"],
		Message:   prefs.Interpolate(opts.Prefs.Templates[typ], vars),
		Timestamp: opts.now(),
	}
	n.PRUrl = vars["prUrl"]
	switch EventType(typ) {
	case EventRepoNewPR, EventRepoNewIssue:
		if len(ev.RepoItems) > 0 {
			n.Detail = repoItemsDetail(ev.RepoItems, typ)
		}
	}
	return n
}

func buildVarsRepo(id resolver.Identity, status *RepoStatus, ev Event, interval time.Duration) map[string]string {
	host := id.Host
	if host == "" {
		host = "github.com"
	}
	v := map[string]string{
		"owner":       id.Owner,
		"repo":        id.Repo,
		"host":        host,
		"prLabel":     fmt.Sprintf("%s/%s", id.Owner, id.Repo),
		"prUrl":       fmt.Sprintf("https://%s/%s/%s", host, id.Owner, id.Repo),
		"intervalSec": strconv.Itoa(int(interval.Seconds())),
	}

	if status != nil {
		v["repoPRs"] = strconv.Itoa(len(status.PRs))
		v["repoIssues"] = strconv.Itoa(len(status.Issues))
	}

	// For individual item events, set the item-specific vars.
	if len(ev.RepoItems) > 0 {
		item := ev.RepoItems[0]
		v["repoItemNumber"] = strconv.Itoa(item.Number)
		v["repoItemTitle"] = item.Title
		v["repoItemAuthor"] = item.Author
		v["repoItemUrl"] = item.URL

		itemLabel := fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, item.Number)
		v["prLabel"] = itemLabel
		v["prUrl"] = item.URL
	}

	return v
}

// repoItemsDetail renders a detail body for one or more repo items.
func repoItemsDetail(items []RepoItemSummary, typ string) string {
	parts := make([]string, 0, len(items))
	kind := "PR"
	if typ == string(EventRepoNewIssue) {
		kind = "issue"
	}
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("New %s #%d: %s (by %s)\n  %s", kind, it.Number, it.Title, it.Author, it.URL))
	}
	return strings.Join(parts, "\n\n")
}
