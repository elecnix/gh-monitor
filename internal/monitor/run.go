package monitor

import (
	"encoding/json"
	"fmt"
	"math/rand"
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
// MaxIdleInterval is exported so the shared poller daemon can cap its
// budget-stretched cadence with the same ceiling.
const (
	MaxIdleInterval = 300 * time.Second
	maxIdleInterval = MaxIdleInterval
	maxErrBackoff   = 300 * time.Second

	// defaultInterval is the built-in poll cadence (issue #90): a slow
	// trickle that only exists as insurance against event loss. The old 60s
	// default was the aggressive part of the safety net — under a healthy
	// broker, timer polling adds no freshness that wakes do not already
	// deliver, so it was pure API spend.
	defaultInterval = 5 * time.Minute
)

// MaxFailedLogLines caps the failed-run log snippet embedded in a
// run-completed notification (issue #19). `gh run view --log-failed` emits the
// full failed-step log chronologically, with the actual error at the END, so we
// keep the last N lines (the error + its immediate context) rather than the
// first N — taking the head would capture only setup noise and miss the error.
// It is exported so the shared poller daemon caps its snippets exactly as the
// loops that preceded the shared poller did.
const MaxFailedLogLines = 50

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

// SummarizeFailedLog cleans and tail-truncates the failed-run log output so
// the snippet carries the error (which lives at the end of the `--log-failed`
// output) plus its immediate context. When the cleaned log fits within
// maxLines it is returned verbatim; otherwise the last maxLines lines are
// kept, prefixed by a one-line marker noting how many earlier lines were
// dropped. Empty input yields "". It is exported so the shared poller daemon
// summarizes snippets exactly as the watch path always has.
func SummarizeFailedLog(s string, maxLines int) string {
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

	// AnnotationLevels controls which check-run annotation levels are
	// included in a PR snapshot's annotations. A nil value uses the default
	// (warning + failure). An empty filter ("none") drops all annotations.
	// It is read configuration: the built-in backend's Reader applies it,
	// and the daemon parses its own from WatchOptions.
	AnnotationLevels *AnnotationLevels

	// Now is injectable for tests. It defaults to time.Now.
	Now func() time.Time

	// SaveSnapshot is called after each successful poll with a JSON-serialised
	// PRStatus or IssueStatus. The caller persists this to the cursor store.
	// It is NOT called on degraded (error) fetches — a cursor must not advance
	// past events that were never actually read.
	SaveSnapshot func(snapshotJSON string)
}

func (o *RunOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Jittered spreads d by a uniform ±20%. It is exported so the shared poller
// daemon can de-phase its per-target tickers. math/rand is fine here: the value shapes request
// timing, not security.
func Jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := d / 5
	offset := time.Duration(rand.Int63n(2*int64(span)+1)) - span
	return d + offset
}

// ---------------------------------------------------------------------------
// PR target
// ---------------------------------------------------------------------------

// ciAllGreen reports whether an open PR's CI has finished with every check
// passing. It requires at least one successful check: with no checks at all —
// the state GitHub reports for the first seconds after a push, and the
// permanent state of a repo without CI — FailingChecks and PendingChecks are
// empty too, and announcing green there would be a guess, not an observation.
//
// When AwaitingChecks is non-empty, CI is not green: required checks from the
// branch ruleset have not yet been created. When RulesetError is non-empty,
// the required set is unknown and the monitor stays quiet rather than guessing.
// When TruncatedSuites is true, the payload is incomplete and the monitor
// cannot confirm that all checks ran.
func ciAllGreen(s *PRStatus) bool {
	return !s.Merged && s.State != "CLOSED" &&
		len(s.FailingChecks) == 0 && len(s.PendingChecks) == 0 &&
		len(s.AwaitingChecks) == 0 && s.RulesetError == "" &&
		!s.TruncatedSuites &&
		len(s.SuccessfulChecks) > 0
}

// ---------------------------------------------------------------------------
// Ref / commit target
// ---------------------------------------------------------------------------


// ---------------------------------------------------------------------------
// Issue target
// ---------------------------------------------------------------------------


// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// IdleInterval returns the poll interval given the number of consecutive
// no-change polls: base until 3 no-change polls, then base*2^(n-3) capped at
// maxIdleInterval. It is exported so the shared poller daemon can back its
// per-target cadence off a single shared formula.
func IdleInterval(base time.Duration, noChange int) time.Duration {
	return IdleIntervalCapped(base, noChange, maxIdleInterval)
}

// IdleIntervalCapped is IdleInterval with a caller-supplied ceiling instead
// of the fixed MaxIdleInterval. It exists for the daemon's optional broker
// transport (see internal/hub and internal/broker): while that transport
// reports healthy, a poller's idle backoff is allowed to grow well past the
// normal 300s ceiling, because a real change now arrives as an immediate
// wake instead of waiting for the next tick — polling becomes a rare safety
// net, not the primary path. cap <= 0 is treated as "no ceiling" (the
// backoff still starts at base and only grows, so this is never used to
// stop polling altogether).
func IdleIntervalCapped(base time.Duration, noChange int, cap time.Duration) time.Duration {
	d := base
	if noChange >= 3 {
		shift := uint(noChange - 3)
		if shift > 20 {
			shift = 20
		}
		d = base * time.Duration(uint64(1)<<shift)
	}
	if cap > 0 && d > cap {
		d = cap
	}
	if d < base {
		d = base
	}
	return d
}

// NextErrBackoff doubles the current error backoff (starting at base),
// capped at maxErrBackoff. It is exported so the shared poller backs its
// cadence off under consecutive fetch failures exactly as the in-process
// loops did.
func NextErrBackoff(cur, base time.Duration) time.Duration {
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

// saveIssueSnapshot serialises curr and calls opts.SaveSnapshot. It is a no-op
// when SaveSnapshot is nil.
func saveIssueSnapshot(opts RunOptions, curr *IssueStatus) {
	if opts.SaveSnapshot == nil {
		return
	}
	b, err := json.Marshal(curr)
	if err != nil {
		return
	}
	opts.SaveSnapshot(string(b))
}

// ---------------------------------------------------------------------------
// PR notification rendering
// ---------------------------------------------------------------------------

// renderNotificationPR builds the interpolation vars, renders the template for
// typ, and populates the structured fields relevant to the event.
func renderNotificationPR(opts RunOptions, status *PRStatus, typ string, ev Event) Notification {
	vars := buildVarsPR(opts.Identity, status, ev, opts.Interval)
	msg := prefs.Interpolate(opts.Prefs.Templates[typ], vars)

	// Append awaiting-checks info when present — this is not in the template
	// because it is a structured suffix that depends on runtime state.
	if status != nil && len(status.AwaitingChecks) > 0 {
		msg = msg + " | awaiting: " + strings.Join(status.AwaitingChecks, ", ")
	}
	if status != nil && status.RulesetError != "" {
		msg = msg + " | ⚠ ruleset: " + status.RulesetError
	}
	if status != nil && status.TruncatedSuites {
		msg = msg + " | ⚠ check suite list was truncated — snapshot may be incomplete"
	}

	n := Notification{
		Type:      typ,
		PRLabel:   vars["prLabel"],
		Message:   msg,
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
		n.Detail = annotationsDetail(ev.Annotations, ev.AnnotationsTruncated, ev.AnnotationsURL)
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
		vars["awaitingChecks"] = strings.Join(status.AwaitingChecks, ", ")
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
		if ev.AnnotationsTruncated {
			vars["annotationTruncated"] = " (truncated)"
		} else {
			vars["annotationTruncated"] = ""
		}
		vars["annotationUrl"] = ev.AnnotationsURL
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


//
// When opts.Instance is set, the loop uses the per-instance cursor to filter
// out items it has already seen across restarts. The cursor is advanced after
// each poll via opts.AdvanceCursor.
// ---------------------------------------------------------------------------
// Cursor-aware repo filtering (issue #32)
// ---------------------------------------------------------------------------

// ClipRepoResponse returns a copy of resp with every PR and issue created at
// or before threshold (an RFC3339 timestamp) suppressed. It is the shared
// clipping primitive behind the named-instance cursor filter: the in-process
// client passes via WatchOptions.Since, so a repo watch resumed from a
// cursor sees only what came after it.
func ClipRepoResponse(resp *RepoQueryResponse, threshold string) *RepoQueryResponse {
	// Shallow copy and filter nodes.
	filtered := *resp
	filtered.Repository.PullRequests.Nodes = filterRepoPRs(resp.Repository.PullRequests.Nodes, threshold)
	filtered.Repository.Issues.Nodes = filterRepoIssues(resp.Repository.Issues.Nodes, threshold)
	return &filtered
}

func filterRepoPRs(nodes []RepoPR, threshold string) []RepoPR {
	out := make([]RepoPR, 0, len(nodes))
	for _, p := range nodes {
		if p.CreatedAt > threshold {
			out = append(out, p)
		}
	}
	return out
}

func filterRepoIssues(nodes []RepoIssue, threshold string) []RepoIssue {
	out := make([]RepoIssue, 0, len(nodes))
	for _, iss := range nodes {
		if iss.CreatedAt > threshold {
			out = append(out, iss)
		}
	}
	return out
}

// LatestRepoCreatedAt returns the most recent CreatedAt timestamp among all
// PRs and issues in the response. It returns "" when the response has no
// items, so the caller leaves the cursor unchanged. It is exported because
// the shared poller stamps it onto every update it emits for a repo target
// so the client can advance its cursor the way the original runRepo loop did.
func LatestRepoCreatedAt(resp *RepoQueryResponse) string {
	var latest string
	for _, p := range resp.Repository.PullRequests.Nodes {
		if p.CreatedAt > latest {
			latest = p.CreatedAt
		}
	}
	for _, iss := range resp.Repository.Issues.Nodes {
		if iss.CreatedAt > latest {
			latest = iss.CreatedAt
		}
	}
	return latest
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

// ---------------------------------------------------------------------------
// Degradation helpers (issue #33)
// ---------------------------------------------------------------------------

// degradedLabel builds a label for the degraded event.
func degradedLabel(opts RunOptions) string {
	id := opts.Identity
	switch id.Target {
	case "ref":
		return fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.Ref)
	case "commit":
		label := fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.CommitSHA)
		if len(id.CommitSHA) > 7 {
			label = fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.CommitSHA[:7])
		}
		return label
	case "issue":
		return fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, id.Number)
	case "run":
		return fmt.Sprintf("%s/%s run #%d", id.Owner, id.Repo, id.RunID)
	case "repo":
		return fmt.Sprintf("%s/%s", id.Owner, id.Repo)
	default:
		return fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, id.Number)
	}
}
