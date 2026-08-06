package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventFilter_RunIntegration confirms the filter is wired into Run's emit
// boundary end-to-end: a scripted PR that goes clean→failing→merged, with an
// allowlist of {merged}, must emit ONLY the merged notification and suppress
// first-poll, ci-all-green, and new-failing-checks.
func TestEventFilter_RunIntegration(t *testing.T) {
	clean := mkPR("OPEN", false, "aaaaaaa", nil)
	// add a successful check so ci-all-green would fire on first poll
	clean.Commits.Nodes[0].Commit.CheckSuites.Nodes[0].CheckRuns = RunNodes{Nodes: []CheckRun{
		{Name: "build", Conclusion: "SUCCESS", Status: "COMPLETED"},
	}}
	failing := mkPR("OPEN", false, "aaaaaaa", []string{"build"})
	merged := mkPR("MERGED", true, "aaaaaaa", []string{"build"})

	svc := &Service{API: scriptedAPI([]*PullRequest{clean, failing, merged, merged})}

	opts := testRunOptions()
	opts.Timeout = 150 * time.Millisecond
	opts.EventFilter = NewEventFilter("merged")

	var got []Notification
	err := Run(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	assert.Equal(t, []string{"merged"}, typesOf(got),
		"with allowlist {merged}, only the merged notification must be emitted")
}
