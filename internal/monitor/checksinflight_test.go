package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// inFlightSuite is a suite GitHub reports while one of its runs has not
// finished: the suite and the run both carry a non-terminal status and a null
// conclusion.
func inFlightSuite(status string) CheckSuite {
	run := CheckRun{Name: "build", Status: status}
	return CheckSuite{
		Status:    status,
		App:       AppInfo{Name: "GitHub Actions", Slug: "github-actions"},
		CheckRuns: RunNodes{Nodes: []CheckRun{run}, TotalCount: 1},
	}
}

func TestChecksInFlight(t *testing.T) {
	settled := checkSuite("GitHub Actions", "github-actions", "SUCCESS", greenRun("build", ts(0)))

	// Every CheckStatusState except COMPLETED is still in flight (AGENTS.md).
	for _, status := range []string{"IN_PROGRESS", "PENDING", "QUEUED", "REQUESTED", "WAITING"} {
		t.Run("a check run in "+status, func(t *testing.T) {
			v := view([]CheckSuite{settled, inFlightSuite(status)}, nil)
			assert.True(t, ChecksInFlight(v))
		})
	}

	t.Run("a pending run under a suite that reads completed", func(t *testing.T) {
		// A rerun starts a run before the suite's own status follows it.
		suite := checkSuite("GitHub Actions", "github-actions", "SUCCESS",
			greenRun("build", ts(0)), CheckRun{Name: "lint", Status: "QUEUED"})
		assert.True(t, ChecksInFlight(view([]CheckSuite{suite}, nil)))
	})

	for _, state := range []string{"PENDING", "EXPECTED"} {
		t.Run("a status context in "+state, func(t *testing.T) {
			status := &CommitStatus{Contexts: []StatusContext{
				{State: "SUCCESS", Context: "ci/lint"},
				{State: state, Context: "ci/deploy"},
			}}
			assert.True(t, ChecksInFlight(view([]CheckSuite{settled}, status)))
		})
	}

	t.Run("every check finished", func(t *testing.T) {
		status := &CommitStatus{Contexts: []StatusContext{{State: "FAILURE", Context: "ci/deploy"}}}
		red := checkSuite("GitHub Actions", "github-actions", "FAILURE", failingRun("test", ts(0)))
		assert.False(t, ChecksInFlight(view([]CheckSuite{settled, red}, status)))
	})

	t.Run("a queued suite that never posted a run", func(t *testing.T) {
		// GitHub creates one suite per installed App on every push. An App
		// that posts no run leaves its suite queued for good, so it is not
		// something a watcher waits on.
		idle := CheckSuite{Status: "QUEUED", App: AppInfo{Name: "Dependabot", Slug: "dependabot"}}
		assert.False(t, ChecksInFlight(view([]CheckSuite{settled, idle}, nil)))
	})

	t.Run("no checks at all", func(t *testing.T) {
		assert.False(t, ChecksInFlight(HeadCommitView{}))
		assert.False(t, ChecksInFlight(HeadCommitViewOf(nil)))
	})
}
