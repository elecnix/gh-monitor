package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shape of elecnix/your-attention run 43908747360 (2026-11-16): the Cite
// review workflow ended in startup_failure. GitHub made a check suite for it
// with zero check runs, so the PR rollup showed only the other workflows, green.
const (
	startupRunURL = "https://github.com/elecnix/your-attention/actions/runs/43908747360"
	startupName   = "Cite review"
)

// startupFailedSuite is the empty suite GitHub leaves behind for that run.
func startupFailedSuite() CheckSuite {
	s := checkSuite("GitHub Actions", "github-actions", "STARTUP_FAILURE")
	s.WorkflowRun = &WorkflowRunRef{URL: startupRunURL}
	s.WorkflowRun.Workflow.Name = startupName
	return s
}

func startupView(extra ...CheckSuite) HeadCommitView {
	suites := []CheckSuite{
		checkSuite("GitHub Actions", "github-actions", "SUCCESS", greenRun("build", ts(0))),
	}
	return view(append(suites, extra...), nil)
}

func TestJudgeCI_StartupFailureIsAFailingCheckThatNamesTheRun(t *testing.T) {
	v := JudgeCI(startupView(startupFailedSuite()), CIVerdictOptions{})

	require.Len(t, v.Failing, 1)
	assert.Contains(t, v.Failing[0], startupName)
	assert.Contains(t, v.Failing[0], startupRunURL)
	assert.Contains(t, v.Failing[0], "never started")
	assert.Contains(t, v.Failing[0], "will not arrive")
	assert.Equal(t, []string{"build"}, v.Successful, "the green workflow stays green")
}

func TestJudgeCI_StartupFailureWithoutWorkflowRunStillFails(t *testing.T) {
	s := checkSuite("GitHub Actions", "github-actions", "STARTUP_FAILURE")
	v := JudgeCI(startupView(s), CIVerdictOptions{})

	require.Len(t, v.Failing, 1)
	assert.Contains(t, v.Failing[0], "never started")
}

func TestJudgeCI_EmptyCompletedActionsSuiteThatFailedNeverStarted(t *testing.T) {
	for _, conclusion := range []string{"FAILURE", "TIMED_OUT", "ACTION_REQUIRED"} {
		v := JudgeCI(startupView(checkSuite("GitHub Actions", "github-actions", conclusion)), CIVerdictOptions{})
		require.Len(t, v.Failing, 1, conclusion)
		assert.Contains(t, v.Failing[0], "never started", conclusion)
	}
}

func TestJudgeCI_EmptyActionsSuitesThatAreNotFailuresStayQuiet(t *testing.T) {
	// #96 and #113: empty CANCELLED container suites are superseded attempts.
	for _, conclusion := range []string{"CANCELLED", "SKIPPED", "SUCCESS", "NEUTRAL"} {
		v := JudgeCI(startupView(checkSuite("GitHub Actions", "github-actions", conclusion)), CIVerdictOptions{})
		assert.Empty(t, v.Failing, conclusion)
	}
}

func TestJudgeCI_StartupFailureWithRunsDefersToTheRuns(t *testing.T) {
	s := checkSuite("GitHub Actions", "github-actions", "STARTUP_FAILURE", greenRun("build", ts(0)))
	v := JudgeCI(view([]CheckSuite{s}, nil), CIVerdictOptions{})
	assert.Empty(t, v.Failing)
}

func TestDiff_ReportsAStartupFailureOncePerRun(t *testing.T) {
	snap := func(suites ...CheckSuite) *PRStatus {
		v := JudgeCI(startupView(suites...), CIVerdictOptions{})
		return &PRStatus{State: "OPEN", FailingChecks: v.Failing, SuccessfulChecks: v.Successful}
	}
	green := snap()
	failed := snap(startupFailedSuite())

	events := Diff(green, failed)
	require.Len(t, events, 1)
	assert.Equal(t, EventNewFailingChecks, events[0].Type)
	require.Len(t, events[0].Checks, 1)
	assert.Contains(t, events[0].Checks[0], startupRunURL)

	assert.Empty(t, Diff(failed, snap(startupFailedSuite())), "the same run is not reported twice")

	rerun := startupFailedSuite()
	rerun.WorkflowRun.URL = startupRunURL + "/attempts/2"
	again := Diff(failed, snap(rerun))
	require.Len(t, again, 1, "a new run is a new report")
	assert.Equal(t, EventNewFailingChecks, again[0].Type)
}
