package monitor

import (
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The CI verdict.
//
// Every rule that answers "is CI green" lives here, over one reader-neutral
// input. Before this module the rules were spread across three call sites and
// three shapes: the PR classifiers took a *PullRequest, so a reader holding
// only a commit had to fabricate one (commitChecks), and each transport had
// to produce a GraphQL-shaped payload before it could be judged at all
// (restCheckSuites upper-cased every enum so the classifiers could not tell
// REST from GraphQL).
//
// A HeadCommitView is the thing every transport can actually produce: the
// check suites attached to the commit under test, with their runs, plus the
// commit's old-style status contexts. GraphQL, REST and the bare-commit
// reader all adapt into it, and JudgeCI is the single entry point over it.
// ---------------------------------------------------------------------------

// CommitChecks is the CI payload of one commit: its check suites (with their
// runs) and its old-style status contexts.
type CommitChecks struct {
	Suites SuiteNodes
	Status *CommitStatus
}

// HeadCommitView is the CI payload a verdict is read from. It carries one
// entry per commit in the payload; every reader in this package populates it
// with the head commit alone (the GraphQL queries all ask for
// commits(last: 1)), and the slice exists so the view does not have to
// re-derive which commit is the head.
type HeadCommitView struct {
	Commits []CommitChecks
}

// HeadCommitViewOf adapts a pull-request payload into the view. This is the
// GraphQL reader's adapter.
func HeadCommitViewOf(pr *PullRequest) HeadCommitView {
	if pr == nil {
		return HeadCommitView{}
	}
	v := HeadCommitView{Commits: make([]CommitChecks, 0, len(pr.Commits.Nodes))}
	for i := range pr.Commits.Nodes {
		c := &pr.Commits.Nodes[i].Commit
		v.Commits = append(v.Commits, CommitChecks{Suites: c.CheckSuites, Status: c.Status})
	}
	return v
}

// HeadCommitViewOfSuites adapts a bare commit's suite/status payload into the
// view. This is the ref and commit readers' adapter — the one that previously
// had to build a synthetic *PullRequest to reach the classifiers.
func HeadCommitViewOfSuites(suites SuiteNodes, status *CommitStatus) HeadCommitView {
	return HeadCommitView{Commits: []CommitChecks{{Suites: suites, Status: status}}}
}

// CIVerdictOptions configures JudgeCI.
type CIVerdictOptions struct {
	// AnnotationLevels filters which check-run annotation levels are
	// collected. A nil value uses the default (warning + failure); an empty
	// filter ("none") drops all annotations.
	AnnotationLevels *AnnotationLevels

	// RequiredChecks are the branch-ruleset contexts that must exist for
	// CI to be green. Empty means the ruleset was not available to this
	// reader and no awaiting set is computed.
	RequiredChecks []string
}

// CIVerdict is the complete CI answer for one head commit: which checks are
// red, which are in flight, which are the positive evidence CI ran, which
// required ones have not been created yet, and how complete the payload was.
type CIVerdict struct {
	Failing    []string
	Pending    []string
	Successful []string
	Awaiting   []string

	// Truncated is true when the payload dropped suites or runs, so the
	// verdict is incomplete and must not be read as "nothing else ran".
	Truncated bool

	Annotations          []AnnotationSummary
	AnnotationsTruncated bool
	AnnotationsURL       string
}

// JudgeCI computes the CI verdict for a head-commit view. It is the single
// entry point: every classifier below is reached only from here, so no reader
// can apply a different version of "is CI green".
func JudgeCI(v HeadCommitView, opts CIVerdictOptions) CIVerdict {
	anns, truncated, url := extractAnnotations(v, opts.AnnotationLevels)
	return CIVerdict{
		Failing:              failingChecks(v),
		Pending:              pendingChecks(v),
		Successful:           successfulChecks(v),
		Awaiting:             awaitingChecks(v, opts.RequiredChecks),
		Truncated:            truncatedSuites(v),
		Annotations:          anns,
		AnnotationsTruncated: truncated,
		AnnotationsURL:       url,
	}
}

// ---------------------------------------------------------------------------
// Verdict tables
//
// These are GitHub wire enums, normalised to the upper-case form every
// transport in this package stores. They are the ONLY place a conclusion or a
// check status is classified, so a value added to GitHub's enums is one line
// in one file rather than a grep across the tree.
// ---------------------------------------------------------------------------

var failureConclusions = map[string]bool{
	"FAILURE": true, "ERROR": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true,
}

// nonVerdictConclusions are terminal conclusions that are NOT results: the run was
// superseded or deliberately not executed, so it never overrides a verdict, whichever
// is newer. Measured 2026-08-18: a name carrying a cancelled run beside a successful
// one was reported as FAILING — the cancelled row was classified per-run instead of
// per name. The mirror trap (a newer skipped hiding an older failure) is the fleet's
// laundered-red case. One rule covers both signs: a non-verdict never overrides a
// verdict, in either direction.
var nonVerdictConclusions = map[string]bool{
	"SKIPPED": true, "CANCELLED": true, "STALE": true,
}

// successConclusions are the terminal conclusions that count as "this check
// passed" — SKIPPED and NEUTRAL are not failures and nothing more will happen
// to them, so they settle the check just as SUCCESS does.
var successConclusions = map[string]bool{
	"SUCCESS": true, "NEUTRAL": true, "SKIPPED": true,
}

// pendingStatuses covers every CheckStatusState except COMPLETED (plus the
// legacy STARTUP_FAILURE entry). A suite matching neither this map nor
// failureConclusions reads as settled, so omitting a non-terminal status here
// reports CI as passing while it is still queued.
var pendingStatuses = map[string]bool{
	"IN_PROGRESS": true, "QUEUED": true, "WAITING": true, "REQUESTED": true, "PENDING": true,
	"STARTUP_FAILURE": true,
}

var failureCommitStates = map[string]bool{"FAILURE": true, "ERROR": true}

var pendingCommitStates = map[string]bool{"PENDING": true, "EXPECTED": true}

var successCommitStates = map[string]bool{"SUCCESS": true}

// isFailureVerdict reports whether a VERDICT conclusion is a failure. CANCELLED is
// deliberately absent: it is a non-verdict (superseded attempt), used only as a
// fallback when the name has no verdict at all.
func isFailureVerdict(c string) bool {
	switch c {
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return true
	}
	return false
}

func isFailureConclusion(c string) bool { return failureConclusions[c] }
func isSuccessConclusion(c string) bool { return successConclusions[c] }
func isPendingStatus(s string) bool     { return pendingStatuses[s] }

// runVerdict selects the newest VERDICT among a name's runs. A non-verdict
// (skipped/cancelled/stale) never overrides a verdict, whichever is newer; AMONG
// VERDICTS the latest wins, so a re-review that found a defect is the real verdict,
// not the earlier green. A run lacking a parseable completion time sorts oldest (it
// cannot prove it is newer), and document order breaks ties.
func runVerdict(runs []CheckRun) (CheckRun, bool) {
	var best CheckRun
	found := false
	var bestT time.Time
	for i := range runs {
		r := &runs[i]
		if r.Status != "COMPLETED" || nonVerdictConclusions[r.Conclusion] {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.CompletedAt)
		if err != nil {
			t = time.Time{} // unparseable cannot prove it is newer
		}
		if !found || t.After(bestT) {
			best = *r
			bestT = t
			found = true
		}
	}
	return best, found
}

// ---------------------------------------------------------------------------
// Suite shape
// ---------------------------------------------------------------------------

// suiteName resolves a display name for a check suite.
func suiteName(s *CheckSuite) string {
	if s.App.Name != "" {
		return s.App.Name
	}
	return s.App.Slug
}

// suiteCarriesRuns reports whether a suite has at least one check run attached.
//
// A suite with NO runs is never a verdict on its own. GitHub leaves runs
// attached to the suite that created them, and keeps that suite's conclusion —
// a superseded attempt's suite reads CANCELLED only when the attempt had no
// runs to conclude, i.e. it is the empty container suite the GitHub Actions app
// materialises per workflow. Those suites all share the container app name, so
// reading one as a result manufactures a check named "GitHub Actions" that no
// run backs and that never clears. Classifying by the app name is the same
// phantom #96 fixed for the with-runs shape; this is the empty-suite shape of
// it (measured live 2026-09-22: 7 empty CANCELLED "GitHub Actions" suites, every
// run SUCCESS or SKIPPED, and the monitor still reported a failing check).
//
// The cost is the opposite phantom, accepted deliberately: a cancelled required
// check that never produced a run reads as absent rather than red, which lands
// it in AwaitingChecks and still holds CI out of green.
func suiteCarriesRuns(s *CheckSuite) bool { return len(s.CheckRuns.Nodes) > 0 }

// containerApps are the apps GitHub uses as a CONTAINER for check runs, never
// as the check itself: the app runs workflows and each run carries its own job
// name. A suite from one of these that carries no runs has nothing to report,
// so its own conclusion is not a result. Matched on slug and name, because the
// tests build suites by name and the API offers both.
var containerApps = map[string]bool{
	"github-actions": true,
	"github actions": true,
}

func isContainerApp(s *CheckSuite) bool {
	return containerApps[strings.ToLower(s.App.Slug)] || containerApps[strings.ToLower(s.App.Name)]
}

// ---------------------------------------------------------------------------
// The five classifiers
// ---------------------------------------------------------------------------

// failingChecks collects names of failing check suites/runs plus old-style
// status contexts in FAILURE/ERROR states.
func failingChecks(v HeadCommitView) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	// Per-name across ALL suites on the head: a cancelled run beside a successful one
	// for the same name is a superseded attempt, not a verdict. Classification is per
	// NAME across all runs, never per run (measured 2026-08-18).
	byName := map[string][]CheckRun{}
	for _, c := range v.Commits {
		for j := range c.Suites.Nodes {
			suite := &c.Suites.Nodes[j]
			// A suite's own conclusion is a result ONLY when the suite carries no
			// runs — a lone non-container check (e.g. the "CI" app) that concluded
			// CANCELLED and produced nothing. Where runs exist, classification
			// defers to them per name; where the app is the container, an empty
			// suite is not a check at all (see suiteCarriesRuns).
			if isFailureConclusion(suite.Conclusion) && !suiteCarriesRuns(suite) && !isContainerApp(suite) {
				add(suiteName(suite))
			}
			for _, run := range suite.CheckRuns.Nodes {
				name := run.Name
				if name == "" {
					name = suiteName(suite)
				}
				if name != "" {
					byName[name] = append(byName[name], run)
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if failureCommitStates[ctx.State] {
					add(ctx.Context)
				}
			}
		}
	}
	for name, runs := range byName {
		if ver, ok := runVerdict(runs); ok {
			if isFailureVerdict(ver.Conclusion) {
				add(name)
			}
			continue
		}
		// No verdict at all: fall back to any failure-shaped conclusion (a cancelled
		// suite that ran and was the only record still reads red, never green).
		for _, r := range runs {
			if isFailureConclusion(r.Conclusion) {
				add(name)
				break
			}
		}
	}
	return out
}

// successfulChecks collects names of check suites/runs that finished without
// failing, plus old-style status contexts in the SUCCESS state.
//
// This is the positive evidence that CI ran: failingChecks and pendingChecks
// are both empty whether every check passed or no check has been created yet,
// and only the former should be reported as green.
func successfulChecks(v HeadCommitView) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	// Same per-name rule as failingChecks: the latest VERDICT decides. A name whose
	// latest verdict is a failure is NOT successful even if an earlier run passed.
	byName := map[string][]CheckRun{}
	for _, c := range v.Commits {
		for j := range c.Suites.Nodes {
			suite := &c.Suites.Nodes[j]
			// Mirror of the failingChecks rule: a suite's own conclusion is a result
			// only when it carries no runs and is not the container app. A SUCCESS
			// container suite would otherwise pad SuccessfulChecks with the app
			// name, and every PR on GitHub has such a suite.
			if isSuccessConclusion(suite.Conclusion) && !suiteCarriesRuns(suite) && !isContainerApp(suite) {
				add(suiteName(suite))
			}
			for _, run := range suite.CheckRuns.Nodes {
				name := run.Name
				if name == "" {
					name = suiteName(suite)
				}
				if name != "" {
					byName[name] = append(byName[name], run)
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if successCommitStates[ctx.State] {
					add(ctx.Context)
				}
			}
		}
	}
	for name, runs := range byName {
		if ver, ok := runVerdict(runs); ok {
			if isSuccessConclusion(ver.Conclusion) {
				add(name)
			}
			continue
		}
		for _, r := range runs {
			if isSuccessConclusion(r.Conclusion) {
				add(name)
				break
			}
		}
	}
	return out
}

// pendingChecks collects names of pending check suites plus old-style status
// contexts in PENDING/EXPECTED states.
func pendingChecks(v HeadCommitView) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, c := range v.Commits {
		for j := range c.Suites.Nodes {
			suite := &c.Suites.Nodes[j]
			// Same rule as the other two classifiers: an empty container suite is
			// not a check. GitHub materialises one per workflow, runless, before
			// its jobs exist. Where the suite DOES carry runs, the app name is
			// still reported — imprecise, but the verdict it produces is right,
			// because a run is genuinely in flight and the suite clears when it
			// concludes.
			if isPendingStatus(suite.Status) && (!isContainerApp(suite) || suiteCarriesRuns(suite)) {
				add(suiteName(suite))
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if pendingCommitStates[ctx.State] {
					add(ctx.Context)
				}
			}
		}
	}
	return out
}

// truncatedSuites reports whether the check-suites payload was truncated —
// the API reported more suites than were returned in nodes. When true, the
// verdict is degraded: awaiting checks may report checks as absent that
// actually ran in the dropped suites.
func truncatedSuites(v HeadCommitView) bool {
	for _, c := range v.Commits {
		if c.Suites.TotalCount > len(c.Suites.Nodes) {
			return true
		}
		for j := range c.Suites.Nodes {
			suite := &c.Suites.Nodes[j]
			if suite.CheckRuns.TotalCount > len(suite.CheckRuns.Nodes) {
				return true
			}
		}
	}
	return false
}

// allPresentCheckNames collects every check name visible in the payload —
// suite names, individual run names, and old-style status context names.
func allPresentCheckNames(v HeadCommitView) map[string]bool {
	names := map[string]bool{}
	for _, c := range v.Commits {
		for j := range c.Suites.Nodes {
			suite := &c.Suites.Nodes[j]
			if sn := suiteName(suite); sn != "" {
				names[sn] = true
			}
			for _, run := range suite.CheckRuns.Nodes {
				if run.Name != "" {
					names[run.Name] = true
				}
			}
		}
		if c.Status != nil {
			for _, ctx := range c.Status.Contexts {
				if ctx.Context != "" {
					names[ctx.Context] = true
				}
			}
		}
	}
	return names
}

// awaitingChecks returns required context names that are entirely absent from
// the check-suites/status payload. A check that is present but not successful
// is still tracked by failingChecks / pendingChecks — awaiting means the check
// has not been created at all.
func awaitingChecks(v HeadCommitView, required []string) []string {
	present := allPresentCheckNames(v)
	var out []string
	seen := map[string]bool{}
	for _, ctx := range required {
		if present[ctx] {
			continue
		}
		if !seen[ctx] {
			seen[ctx] = true
			out = append(out, ctx)
		}
	}
	return out
}

// extractAnnotations collects annotations from all check runs across all
// check suites of the head commit, filtered by levels. It also detects
// truncation: when any check run has totalCount >= 10 (the per-step GitHub
// cap) or totalCount > len(nodes) (our first: 50 page is full), the returned
// truncated flag is true and the URL points to the first such run's permalink.
func extractAnnotations(v HeadCommitView, levels *AnnotationLevels) (annotations []AnnotationSummary, truncated bool, url string) {
	var out []AnnotationSummary
	seen := map[string]bool{}
	for _, c := range v.Commits {
		for j := range c.Suites.Nodes {
			suite := &c.Suites.Nodes[j]
			for _, run := range suite.CheckRuns.Nodes {
				runAnns := run.Annotations
				if runAnns.TotalCount >= 10 && !truncated {
					truncated = true
					url = run.Permalink
				}
				if runAnns.TotalCount > len(runAnns.Nodes) && !truncated {
					truncated = true
					if url == "" {
						url = run.Permalink
					}
				}
				for _, ann := range runAnns.Nodes {
					if !levels.Allows(ann.Level) {
						continue
					}
					s := AnnotationSummary{
						CheckName: run.Name,
						Path:      ann.Path,
						Line:      ann.Location.Start.Line,
						Level:     ann.Level,
						Title:     ann.Title,
						Message:   ann.Message,
					}
					key := annotationKey(s)
					if !seen[key] {
						seen[key] = true
						out = append(out, s)
					}
				}
			}
		}
	}
	return out, truncated, url
}
