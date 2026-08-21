package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFailedRunLogs builds a FailedRunLogsFn that returns the given output (and
// nil error), recording the args it was called with.
func fakeFailedRunLogs(out string) (func(owner, repo string, runID int) (string, error), *callLog) {
	c := &callLog{}
	return func(owner, repo string, runID int) (string, error) {
		c.owner = owner
		c.repo = repo
		c.runID = runID
		c.count++
		return out, nil
	}, c
}

type callLog struct {
	owner string
	repo  string
	runID int
	count int
}

func TestRunRun_FailureIncludesFailedLogSnippet(t *testing.T) {
	logs := "admin-ci\tbuild job\t2026-01-01T00:00:00.000Z ##[error]compile failed\nadmin-ci\tbuild job\t2026-01-01T00:00:00.100Z exit code 1"
	fn, calls := fakeFailedRunLogs(logs)
	svc := &Service{
		API: scriptedRunAPI([]*WorkflowRun{
			mkWorkflowRun("in_progress", ""),
			mkWorkflowRun("completed", "failure"),
		}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Equal(t, "failure", completed.Conclusion)
	// The fetcher was called with the run identity.
	assert.Equal(t, 1, calls.count)
	assert.Equal(t, "octo", calls.owner)
	assert.Equal(t, "demo", calls.repo)
	assert.Equal(t, 30433642, calls.runID)
	// The detail embeds the failing job name and the cleaned error snippet
	// (timestamp + ANSI stripped, job name kept).
	assert.Contains(t, completed.Detail, "build job")
	assert.Contains(t, completed.Detail, "##[error]compile failed")
	// The raw timestamp prefix must NOT survive cleaning.
	assert.NotContains(t, completed.Detail, "2026-01-01T00:00:00.000Z")
}

func TestRunRun_AlreadyFailedAtStartupIncludesLogSnippet(t *testing.T) {
	logs := "deploy\tdeploy-prod\t2026-01-01T00:00:00.000Z ##[error]deploy step failed"
	fn, calls := fakeFailedRunLogs(logs)
	svc := &Service{
		API:             scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "failure")}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Equal(t, 1, calls.count)
	assert.Contains(t, completed.Detail, "##[error]deploy step failed")
}

func TestRunRun_FailureLogKeepsTailWithError(t *testing.T) {
	// Mirrors the real-world shape: a long log where the error lives at the END.
	// `gh run view --log-failed` emits setup noise first, then the failure. A
	// naive head-truncation would drop the error; the implementation must keep
	// the tail so the error survives.
	var b strings.Builder
	for i := 0; i < 90; i++ {
		b.WriteString("admin-ci\tbuild\t2026-01-01T00:00:0" + strconv.Itoa(i) + ".000Z setup noise line " + strconv.Itoa(i) + "\n")
	}
	b.WriteString("admin-ci\tbuild\t2026-01-01T00:00:90.000Z target admin: failed to solve: exit code 1\n")
	b.WriteString("admin-ci\tbuild\t2026-01-01T00:00:91.000Z ##[error]Process completed with exit code 1.")
	fn, _ := fakeFailedRunLogs(b.String())
	svc := &Service{
		API:             scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "failure")}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	require.NotEmpty(t, completed.Detail)
	// The error (at the tail) must be present; the earliest setup line must not.
	assert.Contains(t, completed.Detail, "##[error]Process completed with exit code 1.")
	assert.Contains(t, completed.Detail, "target admin: failed to solve")
	assert.NotContains(t, completed.Detail, "setup noise line 0")
	// Truncation marker notes the dropped head.
	assert.Contains(t, completed.Detail, "earlier lines truncated")
}

func TestRunRun_FailureLogTruncatesToLastFiftyLines(t *testing.T) {
	// 120 non-empty lines; the last one is the distinguishable error.
	var b strings.Builder
	for i := 0; i < 119; i++ {
		b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z line " + strconv.Itoa(i) + "\n")
	}
	b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z THE_FINAL_ERROR")
	fn, _ := fakeFailedRunLogs(b.String())
	svc := &Service{
		API:             scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "failure")}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	lines := strings.Split(completed.Detail, "\n")
	// 1 truncation marker + 50 kept (tail) lines.
	assert.Equal(t, MaxFailedLogLines+1, len(lines))
	assert.Contains(t, completed.Detail, "THE_FINAL_ERROR", "the tail must keep the final error line")
	assert.NotContains(t, completed.Detail, "line 0", "the head must be dropped")
}

func TestRunRun_FailureLogFetchErrorOmitsDetail(t *testing.T) {
	svc := &Service{
		API: scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "failure")}),
		FailedRunLogsFn: func(string, string, int) (string, error) {
			return "", errors.New("boom")
		},
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Empty(t, completed.Detail)
}

func TestRunRun_SuccessDoesNotFetchLogs(t *testing.T) {
	fn, calls := fakeFailedRunLogs("should not be called")
	svc := &Service{
		API: scriptedRunAPI([]*WorkflowRun{
			mkWorkflowRun("in_progress", ""),
			mkWorkflowRun("completed", "success"),
		}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)
	assert.Equal(t, 0, calls.count, "logs must not be fetched for a successful run")

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Empty(t, completed.Detail)
}

func TestRunRun_TimedOutConclusionFetchesLogs(t *testing.T) {
	fn, calls := fakeFailedRunLogs("e2e\tjob\t2026-01-01T00:00:00.000Z ##[error]timed out")
	svc := &Service{
		API: scriptedRunAPI([]*WorkflowRun{
			mkWorkflowRun("queued", ""),
			mkWorkflowRun("in_progress", ""),
			mkWorkflowRun("completed", "timed_out"),
		}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)
	assert.Equal(t, 1, calls.count, "logs must be fetched for a timed-out run")
}

func TestRunRun_NilFetcherOmitsDetail(t *testing.T) {
	// No FailedRunLogsFn wired (e.g. older callers): failed run still emits,
	// just without a detail body and without panicking.
	svc := &Service{API: scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "failure")})}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Empty(t, completed.Detail)
}

func TestOnceRun_FailureIncludesFailedLogSnippet(t *testing.T) {
	fn, calls := fakeFailedRunLogs("admin-ci\tjob\t2026-01-01T00:00:00.000Z ##[error]once failed")
	svc := &Service{
		API:             scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "failure")}),
		FailedRunLogsFn: fn,
	}

	var got []Notification
	err := Once(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Equal(t, 1, calls.count)
	assert.Contains(t, completed.Detail, "##[error]once failed")
}

func TestSummarizeFailedLog(t *testing.T) {
	t.Run("empty stays empty", func(t *testing.T) {
		assert.Equal(t, "", SummarizeFailedLog("", 50))
		assert.Equal(t, "", SummarizeFailedLog("   \n  ", 50))
	})

	t.Run("under limit cleaned and joined", func(t *testing.T) {
		in := "admin-ci\tbuild\t2026-01-01T00:00:00.000Z step one\nadmin-ci\tbuild\t2026-01-01T00:00:01.000Z step two\n"
		out := SummarizeFailedLog(in, 50)
		assert.Equal(t, "build\tstep one", strings.Split(out, "\n")[0])
		assert.Equal(t, "build\tstep two", strings.Split(out, "\n")[1])
		assert.NotContains(t, out, "2026-01-01")
	})

	t.Run("over limit keeps tail with leading marker", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 60; i++ {
			b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z line " + strconv.Itoa(i) + "\n")
		}
		out := SummarizeFailedLog(b.String(), 50)
		lines := strings.Split(out, "\n")
		require.Len(t, lines, 51) // marker + 50
		assert.Contains(t, lines[0], "earlier lines truncated")
		assert.Contains(t, lines[0], "10 ")
		// The last kept line is the actual last line of the input.
		assert.Contains(t, lines[50], "line 59")
		// The dropped head is absent.
		assert.NotContains(t, out, "line 0")
	})

	t.Run("exactly at limit no marker", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 50; i++ {
			b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z line " + strconv.Itoa(i) + "\n")
		}
		out := SummarizeFailedLog(b.String(), 50)
		lines := strings.Split(out, "\n")
		assert.Len(t, lines, 50)
		assert.NotContains(t, out, "truncated")
	})
}

func TestCleanFailedLogLine(t *testing.T) {
	t.Run("strips ansi and timestamp, keeps job", func(t *testing.T) {
		// Real ESC byte form.
		in := "admin-ci\tBundle size\t\xef\xbb\xbf2026-07-10T20:54:41.1431776Z \x1b[31mPackage size limit has exceeded by 9.83 kB\x1b[39m"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "Bundle size\tPackage size limit has exceeded by 9.83 kB", out)
		assert.NotContains(t, out, "\x1b[")
		assert.NotContains(t, out, "2026-07-10")
	})

	t.Run("strips caret-notation ansi (gh non-tty form)", func(t *testing.T) {
		// gh run view --log-failed emits `^[[...m` caret notation (no 0x1b) when
		// stdout is not a TTY (e.g. exec.Command). The cleaner must strip it too.
		in := "admin-ci\tBundle size\t2026-07-10T20:54:41.1431776Z ^[[36;1mpnpm size^[[0m done"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "Bundle size\tpnpm size done", out)
		assert.NotContains(t, out, "^[[")
	})

	t.Run("drops the redundant workflow field", func(t *testing.T) {
		in := "admin-ci\tbuild\t2026-01-01T00:00:00.000Z body"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "build\tbody", out)
		assert.NotContains(t, out, "admin-ci")
	})

	t.Run("non-matching line passes through ansi-stripped", func(t *testing.T) {
		in := "\x1b[31mraw line with no tabs\x1b[0m"
		assert.Equal(t, "raw line with no tabs", cleanFailedLogLine(in))
	})

	t.Run("content with embedded tabs is preserved", func(t *testing.T) {
		in := "wf\tjob\t2026-01-01T00:00:00.000Z before\tafter"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "job\tbefore\tafter", out)
	})
}

func TestIsRunFailureConclusion(t *testing.T) {
	for _, c := range []string{"failure", "timed_out", "cancelled", "action_required"} {
		assert.True(t, isRunFailureConclusion(c), c)
	}
	for _, c := range []string{"success", "neutral", "skipped", "stale", ""} {
		assert.False(t, isRunFailureConclusion(c), c)
	}
}
