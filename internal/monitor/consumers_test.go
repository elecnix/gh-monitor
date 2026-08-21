package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/backend"
)

// collectUpdates returns an emit function that records every update it is
// given, plus the slice it appends to.
func collectUpdates() (func(backend.Update), *[]backend.Update) {
	var got []backend.Update
	return func(u backend.Update) { got = append(got, u) }, &got
}

func mkRefStatus(oid string, failing, pending []string) *RefStatus {
	return &RefStatus{Oid: oid, FailingChecks: failing, PendingChecks: pending}
}

func updateTypes(updates []backend.Update) []backend.EventType {
	out := make([]backend.EventType, 0, len(updates))
	for _, u := range updates {
		out = append(out, u.Event.Type)
	}
	return out
}

func TestRefConsumer(t *testing.T) {
	t.Run("first poll emits first-poll and pre-existing failures", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRefConsumer(RunOptions{})
		// No head OID yet: a first poll surfaces failing checks but no commit.
		c.Consume(mkRefStatus("", []string{"lint"}, nil), emit)
		assert.Equal(t, []backend.EventType{backend.EventFirstPoll, backend.EventNewFailingChecks}, updateTypes(*got))
		assert.Equal(t, 0, c.NoChange())
	})

	t.Run("unchanged poll counts as no change", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRefConsumer(RunOptions{})
		c.Consume(mkRefStatus("aaa", nil, nil), emit)
		// First poll against the empty baseline surfaces the head commit.
		require.Equal(t, []backend.EventType{backend.EventFirstPoll, backend.EventNewCommit}, updateTypes(*got))
		c.Consume(mkRefStatus("aaa", nil, nil), emit)
		assert.Len(t, *got, 2)
		assert.Equal(t, 1, c.NoChange())
	})

	t.Run("new commit and all-green emit after work", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRefConsumer(RunOptions{})
		c.Consume(mkRefStatus("aaa", []string{"lint"}, []string{"build"}), emit)
		c.Consume(mkRefStatus("bbb", nil, nil), emit)
		types := updateTypes((*got)[2:]) // skip first-poll + initial failing
		assert.Contains(t, types, backend.EventCIAllGreen)
		assert.Contains(t, types, backend.EventNewCommit)
	})

	t.Run("never terminal", func(t *testing.T) {
		emit, _ := collectUpdates()
		c := NewRefConsumer(RunOptions{})
		assert.False(t, c.Consume(mkRefStatus("aaa", nil, nil), emit))
	})
}

func TestIssueConsumer(t *testing.T) {
	t.Run("first poll emits first-poll and existing comments", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewIssueConsumer(RunOptions{})
		curr := &IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "c1", Author: "a", Body: "hi"}}}
		c.Consume(curr, emit)
		assert.Equal(t, []backend.EventType{backend.EventFirstPoll, backend.EventIssueNewComment}, updateTypes(*got))
	})

	t.Run("closed issue is terminal and emits issue-closed", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewIssueConsumer(RunOptions{})
		require.False(t, c.Consume(&IssueStatus{State: "OPEN"}, emit))
		*got = nil
		require.True(t, c.Consume(&IssueStatus{State: "CLOSED"}, emit))
		assert.Equal(t, []backend.EventType{backend.EventIssueClosed}, updateTypes(*got))
	})

	t.Run("already-closed issue on first poll emits closed once", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewIssueConsumer(RunOptions{})
		require.True(t, c.Consume(&IssueStatus{State: "CLOSED"}, emit))
		assert.Equal(t, []backend.EventType{backend.EventFirstPoll, backend.EventIssueClosed}, updateTypes(*got))
	})

	t.Run("restored baseline suppresses known comments and first-poll", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewIssueConsumer(RunOptions{})
		c.RestoreBaseline(&IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "c1"}}})
		c.Consume(&IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "c1"}}}, emit)
		assert.Empty(t, *got)
	})

	t.Run("saves snapshot after each poll", func(t *testing.T) {
		var saved []string
		opts := RunOptions{}
		opts.SaveSnapshot = func(s string) { saved = append(saved, s) }
		c := NewIssueConsumer(opts)
		c.Consume(&IssueStatus{State: "OPEN"}, func(backend.Update) {})
		c.Consume(&IssueStatus{State: "CLOSED"}, func(backend.Update) {})
		require.Len(t, saved, 2)
	})
}

func TestRunConsumer(t *testing.T) {
	t.Run("in-progress then completed is terminal", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRunConsumer(RunOptions{})
		require.False(t, c.Consume(SnapshotRun(mkWorkflowRun("in_progress", "")), emit))
		*got = nil
		require.True(t, c.Consume(SnapshotRun(mkWorkflowRun("completed", "success")), emit))
		assert.Equal(t, []backend.EventType{backend.EventRunCompleted}, updateTypes(*got))
	})

	t.Run("already-completed run on first poll emits completed", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRunConsumer(RunOptions{})
		require.True(t, c.Consume(SnapshotRun(mkWorkflowRun("completed", "success")), emit))
		assert.Equal(t, []backend.EventType{backend.EventFirstPoll, backend.EventRunCompleted}, updateTypes(*got))
	})

	t.Run("failed completion carries log detail", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRunConsumer(RunOptions{})
		c.FailedLogDetail = func(runID int) string {
			assert.Equal(t, 30433642, runID)
			return "log line"
		}
		c.Consume(SnapshotRun(mkWorkflowRun("in_progress", "")), emit)
		*got = nil
		c.Consume(SnapshotRun(mkWorkflowRun("completed", "failure")), emit)
		require.Len(t, *got, 1)
		assert.Equal(t, "log line", (*got)[0].Event.Detail)
	})

	t.Run("nil FailedLogDetail still emits the completion", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRunConsumer(RunOptions{})
		c.Consume(SnapshotRun(mkWorkflowRun("completed", "failure")), emit)
		assert.Contains(t, updateTypes(*got), backend.EventRunCompleted)
	})
}

func TestRepoConsumer(t *testing.T) {
	t.Run("first poll surfaces pre-existing items", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRepoConsumer(RunOptions{})
		curr := &RepoStatus{PRs: []RepoItemSummary{{Number: 1}}}
		c.Consume(curr, emit)
		assert.Equal(t, []backend.EventType{backend.EventFirstPoll, backend.EventRepoNewPR}, updateTypes(*got))
	})

	t.Run("new PR emits repo-new-pr", func(t *testing.T) {
		emit, got := collectUpdates()
		c := NewRepoConsumer(RunOptions{})
		c.Consume(&RepoStatus{PRs: []RepoItemSummary{{Number: 1}}}, emit)
		*got = nil
		c.Consume(&RepoStatus{PRs: []RepoItemSummary{{Number: 1}, {Number: 2}}}, emit)
		assert.Equal(t, []backend.EventType{backend.EventRepoNewPR}, updateTypes(*got))
	})

	t.Run("never terminal and tracks no-change", func(t *testing.T) {
		emit, _ := collectUpdates()
		c := NewRepoConsumer(RunOptions{})
		curr := &RepoStatus{}
		assert.False(t, c.Consume(curr, emit))
		// The first poll emits first-poll but no diff events, so it counts as
		// a no-change poll exactly as runRepo's inline counter did.
		assert.Equal(t, 1, c.NoChange())
		assert.False(t, c.Consume(curr, emit))
		assert.Equal(t, 2, c.NoChange())
	})
}

func TestKindFingerprints(t *testing.T) {
	t.Run("ref fingerprint covers oid and checks", func(t *testing.T) {
		a := FingerprintRef(mkRefStatus("aaa", []string{"lint"}, nil))
		assert.Equal(t, a, FingerprintRef(mkRefStatus("aaa", []string{"lint"}, nil)))
		assert.NotEqual(t, a, FingerprintRef(mkRefStatus("bbb", []string{"lint"}, nil)))
		assert.NotEqual(t, a, FingerprintRef(mkRefStatus("aaa", nil, nil)))
		assert.Equal(t, "", FingerprintRef(nil))
	})

	t.Run("issue fingerprint covers state and comment ids", func(t *testing.T) {
		a := FingerprintIssue(&IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "c1"}}})
		assert.Equal(t, a, FingerprintIssue(&IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "c1"}}}))
		assert.NotEqual(t, a, FingerprintIssue(&IssueStatus{State: "CLOSED", Comments: []IssueCommentSummary{{ID: "c1"}}}))
		assert.NotEqual(t, a, FingerprintIssue(&IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "c2"}}}))
	})

	t.Run("run fingerprint covers status and conclusion", func(t *testing.T) {
		assert.Equal(t, "completed|success", FingerprintRun(&RunStatus{Status: "completed", Conclusion: "success"}))
		assert.NotEqual(t, FingerprintRun(&RunStatus{Status: "completed", Conclusion: "success"}),
			FingerprintRun(&RunStatus{Status: "completed", Conclusion: "failure"}))
	})

	t.Run("repo fingerprint covers item numbers", func(t *testing.T) {
		a := FingerprintRepo(&RepoStatus{PRs: []RepoItemSummary{{Number: 1}}, Issues: []RepoItemSummary{{Number: 7}}})
		assert.Equal(t, a, FingerprintRepo(&RepoStatus{PRs: []RepoItemSummary{{Number: 1}}, Issues: []RepoItemSummary{{Number: 7}}}))
		assert.NotEqual(t, a, FingerprintRepo(&RepoStatus{PRs: []RepoItemSummary{{Number: 1}, {Number: 2}}}))
	})
}
