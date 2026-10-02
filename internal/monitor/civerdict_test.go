package monitor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixtures. Every terminal value here is one GitHub actually returns: a suite
// that has concluded carries Status "COMPLETED" and one of the six terminal
// CheckConclusion values. A suite with a blank status AND a blank conclusion
// does not exist on the wire — it would read "settled" for the wrong reason,
// which is the fixture mistake AGENTS.md warns about.
//
// The one deliberate exception is the "GitHub Actions" container suite: the
// API does return a runless container suite whose own conclusion is CANCELLED
// while every run under it succeeded (issues #96 and #113). That is a real
// state, not a blank one, so it is exercised as its own fixture below.
// ---------------------------------------------------------------------------

// checkSuite builds a settled suite: COMPLETED plus the given conclusion.
func checkSuite(app, slug, conclusion string, runs ...CheckRun) CheckSuite {
	return CheckSuite{
		Status:     "COMPLETED",
		Conclusion: conclusion,
		App:        AppInfo{Name: app, Slug: slug},
		CheckRuns:  RunNodes{Nodes: runs, TotalCount: len(runs)},
	}
}

// greenRun is a settled, passing check run.
func greenRun(name, completedAt string) CheckRun {
	return CheckRun{Name: name, Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: completedAt}
}

func failingRun(name, completedAt string) CheckRun {
	return CheckRun{Name: name, Status: "COMPLETED", Conclusion: "FAILURE", CompletedAt: completedAt}
}

func ts(offset time.Duration) string {
	return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Add(offset).Format(time.RFC3339)
}

// view builds a single-commit HeadCommitView from suites and an optional
// old-style status payload.
func view(suites []CheckSuite, status *CommitStatus) HeadCommitView {
	return HeadCommitView{Commits: []CommitChecks{{Suites: SuiteNodes{Nodes: suites, TotalCount: len(suites)}, Status: status}}}
}

// ---------------------------------------------------------------------------
// The entry point
// ---------------------------------------------------------------------------

func TestJudgeCI_SettlesEverySurfaceInOnePass(t *testing.T) {
	v := view([]CheckSuite{
		checkSuite("CI", "ci-app", "SUCCESS", greenRun("build", ts(0))),
		checkSuite("e2e", "e2e-app", "FAILURE", failingRun("e2e/spec", ts(time.Minute))),
	}, &CommitStatus{Contexts: []StatusContext{{State: "PENDING", Context: "lint"}}})

	got := JudgeCI(v, CIVerdictOptions{RequiredChecks: []string{"build", "missing-required"}})

	assert.Equal(t, []string{"e2e/spec"}, got.Failing, "a settled FAILURE run is failing")
	assert.Equal(t, []string{"lint"}, got.Pending, "a PENDING status context is in flight")
	assert.Equal(t, []string{"build"}, got.Successful, "a settled SUCCESS run is the positive evidence CI ran")
	assert.Equal(t, []string{"missing-required"}, got.Awaiting, "a required context absent from the payload is awaiting")
	assert.False(t, got.Truncated, "a complete payload is not truncated")
}

func TestJudgeCI_EmptyPayloadIsNotGreen(t *testing.T) {
	// The state GitHub reports for the first seconds after a push, and the
	// permanent state of a repo without CI. All three name lists are empty;
	// only ciAllGreen's "at least one successful check" clause keeps this
	// from being announced as green.
	got := JudgeCI(HeadCommitView{}, CIVerdictOptions{})

	assert.Empty(t, got.Failing)
	assert.Empty(t, got.Pending)
	assert.Empty(t, got.Successful)
	assert.Empty(t, got.Awaiting)
	assert.False(t, got.Truncated)

	status := &PRStatus{State: "OPEN", SuccessfulChecks: got.Successful, FailingChecks: got.Failing, PendingChecks: got.Pending}
	assert.False(t, ciAllGreen(status), "no check at all is not evidence of green")
}

func TestJudgeCI_RequiredChecksAbsentMeansNoAwaiting(t *testing.T) {
	// A reader with no ruleset (ref/commit targets) passes no required set.
	// The verdict must not invent an awaiting entry.
	v := view([]CheckSuite{checkSuite("CI", "ci-app", "SUCCESS", greenRun("build", ts(0)))}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Empty(t, got.Awaiting)
	assert.Equal(t, []string{"build"}, got.Successful)
}

// ---------------------------------------------------------------------------
// Suite-level classification: the container-app rule (issues #96, #113).
// ---------------------------------------------------------------------------

func TestJudgeCI_EmptyContainerSuiteIsNotACheck(t *testing.T) {
	// Measured live: 7 runless CANCELLED "GitHub Actions" suites beside runs
	// that were all SUCCESS or SKIPPED, and the monitor still reported a
	// failing check. An empty container suite has nothing to report.
	v := view([]CheckSuite{
		{Status: "COMPLETED", Conclusion: "CANCELLED", App: AppInfo{Name: "GitHub Actions", Slug: "github-actions"}},
		{Status: "COMPLETED", Conclusion: "SUCCESS", App: AppInfo{Name: "GitHub Actions", Slug: "github-actions"}},
		checkSuite("CI", "ci-app", "SUCCESS", greenRun("build", ts(0))),
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Empty(t, got.Failing, "a runless container suite is not a check and cannot fail")
	assert.NotContains(t, got.Successful, "GitHub Actions", "a runless container suite cannot vouch for CI either")
	assert.Equal(t, []string{"build"}, got.Successful)
}

func TestJudgeCI_EmptyNonContainerSuiteUsesItsOwnConclusion(t *testing.T) {
	// The complement: a lone third-party app that concluded and produced
	// nothing IS a check, and its own conclusion is the result.
	v := view([]CheckSuite{
		{Status: "COMPLETED", Conclusion: "FAILURE", App: AppInfo{Name: "Dependabot", Slug: "dependabot"}},
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Equal(t, []string{"Dependabot"}, got.Failing)
}

func TestJudgeCI_ContainerSuitePendingOnlyWhenItCarriesRuns(t *testing.T) {
	runless := JudgeCI(view([]CheckSuite{
		{Status: "QUEUED", Conclusion: "", App: AppInfo{Name: "GitHub Actions", Slug: "github-actions"}},
	}, nil), CIVerdictOptions{})
	assert.Empty(t, runless.Pending, "a runless container suite is not a check in flight")

	withRuns := JudgeCI(view([]CheckSuite{
		{Status: "QUEUED", Conclusion: "", App: AppInfo{Name: "GitHub Actions", Slug: "github-actions"},
			CheckRuns: RunNodes{Nodes: []CheckRun{{Name: "build", Status: "QUEUED"}}}},
	}, nil), CIVerdictOptions{})
	assert.Equal(t, []string{"GitHub Actions"}, withRuns.Pending, "a container suite with a run genuinely in flight is pending")
}

// ---------------------------------------------------------------------------
// Per-name run verdicts
// ---------------------------------------------------------------------------

func TestJudgeCI_CancelledBesideSuccessfulIsNotFailing(t *testing.T) {
	// Measured 2026-08-18: classification per RUN reported this name as
	// FAILING. Per NAME it is a superseded attempt beside a verdict.
	v := view([]CheckSuite{
		checkSuite("CI", "ci-app", "",
			CheckRun{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: ts(time.Minute)},
			CheckRun{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", CompletedAt: ts(0)}),
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Empty(t, got.Failing, "a non-verdict never overrides a verdict")
	assert.Equal(t, []string{"test"}, got.Successful)
}

func TestJudgeCI_NewerSkippedDoesNotHideOlderFailure(t *testing.T) {
	// The fleet's laundered-red case: a newer SKIPPED run must not wash out
	// the newest FAILURE for the same name.
	v := view([]CheckSuite{
		checkSuite("CI", "ci-app", "",
			failingRun("test", ts(0)),
			CheckRun{Name: "test", Status: "COMPLETED", Conclusion: "SKIPPED", CompletedAt: ts(time.Minute)}),
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Equal(t, []string{"test"}, got.Failing)
}

func TestJudgeCI_NewerFailureOverridesOlderSuccess(t *testing.T) {
	v := view([]CheckSuite{
		checkSuite("CI", "ci-app", "",
			greenRun("test", ts(0)),
			failingRun("test", ts(time.Minute))),
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Equal(t, []string{"test"}, got.Failing)
	assert.NotContains(t, got.Successful, "test", "among verdicts the latest wins, in either direction")
}

func TestJudgeCI_NoVerdictAtAllFallsBackToFailureShapedConclusion(t *testing.T) {
	// Every run for the name was superseded: no verdict exists, so any
	// failure-shaped conclusion still reads red rather than green.
	v := view([]CheckSuite{
		checkSuite("CI", "ci-app", "",
			CheckRun{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", CompletedAt: ts(0)},
			CheckRun{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", CompletedAt: ts(time.Minute)}),
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Equal(t, []string{"test"}, got.Failing)
}

func TestJudgeCI_RunWithoutNameFallsBackToSuiteName(t *testing.T) {
	v := view([]CheckSuite{
		{Status: "COMPLETED", Conclusion: "FAILURE", App: AppInfo{Name: "Semgrep"}, CheckRuns: RunNodes{
			Nodes: []CheckRun{{Status: "COMPLETED", Conclusion: "FAILURE", CompletedAt: ts(0)}}}},
	}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.Equal(t, []string{"Semgrep"}, got.Failing)
}

// ---------------------------------------------------------------------------
// CheckStatusState coverage — all six values, plus the legacy entry.
// ---------------------------------------------------------------------------

func TestJudgeCI_EveryNonCompletedCheckStatusStateIsPending(t *testing.T) {
	// CheckStatusState has six members. Anything other than COMPLETED is
	// still in flight, and a suite matching neither the pending set nor a
	// failure conclusion reads as SETTLED — so omitting one here would report
	// CI as passing while the check is still queued.
	for _, status := range []string{"IN_PROGRESS", "QUEUED", "WAITING", "REQUESTED", "PENDING", "STARTUP_FAILURE"} {
		t.Run(status, func(t *testing.T) {
			v := view([]CheckSuite{
				{Status: status, Conclusion: "", App: AppInfo{Name: "CI", Slug: "ci-app"}},
			}, nil)

			got := JudgeCI(v, CIVerdictOptions{})

			assert.Equal(t, []string{"CI"}, got.Pending, "CheckStatusState %q is in flight", status)
			assert.Empty(t, got.Successful, "an in-flight suite is not evidence of green")
		})
	}
}

// ---------------------------------------------------------------------------
// Degradation
// ---------------------------------------------------------------------------

func TestJudgeCI_DroppedSuitesMarkTheVerdictTruncated(t *testing.T) {
	v := HeadCommitView{Commits: []CommitChecks{{
		Suites: SuiteNodes{
			TotalCount: 51, // the query asks for 50
			Nodes:      []CheckSuite{checkSuite("CI", "ci-app", "SUCCESS", greenRun("build", ts(0)))},
		},
	}}}

	got := JudgeCI(v, CIVerdictOptions{})

	assert.True(t, got.Truncated, "a payload that dropped suites is degraded")
}

func TestJudgeCI_DroppedRunsMarkTheVerdictTruncated(t *testing.T) {
	suite := checkSuite("CI", "ci-app", "SUCCESS", greenRun("build", ts(0)))
	suite.CheckRuns.TotalCount = 11 // only 10 fetched
	v := view([]CheckSuite{suite}, nil)

	assert.True(t, JudgeCI(v, CIVerdictOptions{}).Truncated)
}

// ---------------------------------------------------------------------------
// Annotations
// ---------------------------------------------------------------------------

func TestJudgeCI_AnnotationsHonourTheLevelFilter(t *testing.T) {
	run := greenRun("build", ts(0))
	run.Annotations = AnnotationNodes{TotalCount: 2, Nodes: []Annotation{
		{Level: "WARNING", Title: "unused var", Path: "main.go"},
		{Level: "NOTICE", Title: "generated", Path: "gen.go"},
	}}
	v := view([]CheckSuite{checkSuite("CI", "ci-app", "SUCCESS", run)}, nil)

	got := JudgeCI(v, CIVerdictOptions{AnnotationLevels: NewAnnotationLevels("warning")})

	require.Len(t, got.Annotations, 1)
	assert.Equal(t, "unused var", got.Annotations[0].Title)
	assert.Equal(t, "build", got.Annotations[0].CheckName)
}

func TestJudgeCI_AnnotationsTruncatedAtThePerStepCap(t *testing.T) {
	run := greenRun("build", ts(0))
	run.Permalink = "https://github.com/o/r/runs/1"
	run.Annotations = AnnotationNodes{TotalCount: 10}
	v := view([]CheckSuite{checkSuite("CI", "ci-app", "SUCCESS", run)}, nil)

	got := JudgeCI(v, CIVerdictOptions{})

	assert.True(t, got.AnnotationsTruncated, "totalCount >= 10 implies the per-step cap")
	assert.Equal(t, "https://github.com/o/r/runs/1", got.AnnotationsURL)
}

// ---------------------------------------------------------------------------
// The adapters
// ---------------------------------------------------------------------------

func TestHeadCommitViewOfSuites_MatchesThePullRequestAdapter(t *testing.T) {
	suites := []CheckSuite{
		checkSuite("CI", "ci-app", "", failingRun("build", ts(0)), greenRun("test", ts(time.Minute))),
		{Status: "QUEUED", Conclusion: "", App: AppInfo{Name: "Lint", Slug: "lint-app"}},
	}
	status := &CommitStatus{Contexts: []StatusContext{{State: "ERROR", Context: "verify"}}}

	// The GraphQL PR payload and the bare-commit payload are the same CI
	// state read through two transports; the verdict must not be able to tell.
	fromPR := JudgeCI(HeadCommitViewOf(&PullRequest{
		Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
			CheckSuites: SuiteNodes{Nodes: suites, TotalCount: len(suites)},
			Status:      status,
		}}}},
	}), CIVerdictOptions{RequiredChecks: []string{"verify", "deploy"}})

	fromCommit := JudgeCI(HeadCommitViewOfSuites(SuiteNodes{Nodes: suites, TotalCount: len(suites)}, status),
		CIVerdictOptions{RequiredChecks: []string{"verify", "deploy"}})

	assert.Equal(t, fromPR, fromCommit, "the verdict reads the CI payload, not the transport it arrived on")
	// Run names are grouped through a map, so their order is not stable —
	// only membership is part of the contract.
	assert.ElementsMatch(t, []string{"build", "verify"}, fromPR.Failing)
	assert.Equal(t, []string{"Lint"}, fromPR.Pending)
	assert.Equal(t, []string{"test"}, fromPR.Successful)
	assert.Equal(t, []string{"deploy"}, fromPR.Awaiting)
}

func TestHeadCommitViewOf_EmptyPullRequestYieldsAnEmptyView(t *testing.T) {
	assert.Empty(t, HeadCommitViewOf(nil).Commits)
	assert.Empty(t, HeadCommitViewOf(&PullRequest{}).Commits)
}