package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkCommit(oid string, failing []string) Commit {
	runs := make([]CheckRun, 0, len(failing))
	for _, name := range failing {
		runs = append(runs, CheckRun{Name: name, Conclusion: "FAILURE"})
	}
	return Commit{Commit: CommitDetails{
		Oid:             oid,
		MessageHeadline: "headline",
		CheckSuites:     SuiteNodes{Nodes: []CheckSuite{{App: AppInfo{Name: "CI"}, CheckRuns: RunNodes{Nodes: runs}}}},
	}}
}

func mkPR(state string, merged bool, oid string, failing []string) *PullRequest {
	return &PullRequest{
		State:   state,
		Merged:  merged,
		Commits: CommitNodes{Nodes: []Commit{mkCommit(oid, failing)}},
	}
}

// scriptedAPI returns each response in order, repeating the last one.
func scriptedAPI(responses []*PullRequest) *fakeAPI {
	call := 0
	return &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		idx := call
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		call++
		return assign(result, QueryResponse{Repository: struct {
			PullRequest *PullRequest `json:"pullRequest"`
		}{PullRequest: responses[idx]}})
	}}
}

func testRunOptions() RunOptions {
	return RunOptions{
		Identity: resolver.Identity{Owner: "o", Repo: "r", Number: 7, Host: "github.com"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 60 * time.Second,
		Now:      func() time.Time { return time.Unix(0, 0).UTC() },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
}

func typesOf(ns []Notification) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Type
	}
	return out
}

func TestRun_StreamsEventsUntilMerged(t *testing.T) {
	svc := &Service{API: scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", nil),                // baseline, clean
		mkPR("OPEN", false, "aaaaaaa", []string{"build"}),  // failing appears
		mkPR("MERGED", true, "aaaaaaa", []string{"build"}), // merged (keep failing -> no all-green)
	})}

	var got []Notification
	err := Run(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	require.NotEmpty(t, types)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventNewFailingChecks))
	assert.Equal(t, string(EventMerged), types[len(types)-1])

	var failing *Notification
	for i := range got {
		if got[i].Type == string(EventNewFailingChecks) {
			failing = &got[i]
		}
	}
	require.NotNil(t, failing)
	assert.Equal(t, "❌ Failing CI checks on o/r#7: build", failing.Message)
	assert.Equal(t, []string{"build"}, failing.FailingChecks)
}

func TestRun_NoChangeEmitsNothing(t *testing.T) {
	svc := &Service{API: scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", nil),
		mkPR("OPEN", false, "aaaaaaa", nil), // identical -> no events
		mkPR("MERGED", true, "aaaaaaa", nil),
	})}

	var got []Notification
	err := Run(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	assert.Equal(t, []string{firstPollType, string(EventNewCommit), string(EventCIAllGreen), string(EventMerged)}, typesOf(got))
}

func TestRun_ContextCancelStops(t *testing.T) {
	svc := &Service{API: scriptedAPI([]*PullRequest{mkPR("OPEN", false, "aaaaaaa", nil)})}
	opts := testRunOptions()
	opts.Sleep = func(context.Context, time.Duration) error { return context.Canceled }

	var got []Notification
	err := Run(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	assert.ErrorIs(t, err, context.Canceled)
	require.NotEmpty(t, got)
	assert.Equal(t, firstPollType, got[0].Type) // first-poll emitted before the (cancelling) sleep
}

func TestRun_AlreadyMergedAtStartup(t *testing.T) {
	svc := &Service{API: scriptedAPI([]*PullRequest{mkPR("MERGED", true, "aaaaaaa", nil)})}

	var got []Notification
	err := Run(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)
	// On first poll, diff against empty baseline surfaces the commit + merged state.
	assert.Equal(t, []string{firstPollType, string(EventNewCommit), string(EventMerged)}, typesOf(got))
}

func TestOnce_EmitsCurrentActionable(t *testing.T) {
	pr := &PullRequest{
		State:    "OPEN",
		Comments: CommentNodes{Nodes: []Comment{mkComment("c1", "alice", "please fix", nil)}},
		ReviewThreads: ThreadNodes{Nodes: []ReviewThread{{
			ID:       "t1",
			Comments: CommentNodes{Nodes: []Comment{mkComment("tc1", "bob", "nit", nil)}},
		}}},
		Commits: CommitNodes{Nodes: []Commit{mkCommit("abc1234def", []string{"build"})}},
	}
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}

	var got []Notification
	err := Once(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventNewFailingChecks))
	assert.Contains(t, types, string(EventNewUnresolvedThreads))
	assert.Contains(t, types, string(EventNewGeneralComments))
	assert.Contains(t, types, string(EventNewCommit))
}

func TestIdleInterval(t *testing.T) {
	base := 60 * time.Second
	assert.Equal(t, base, idleInterval(base, 0))
	assert.Equal(t, base, idleInterval(base, 3))              // growth starts after 3
	assert.Equal(t, 2*base, idleInterval(base, 4))            // base * 2^1
	assert.Equal(t, maxIdleInterval, idleInterval(base, 100)) // capped
}

// ---------------------------------------------------------------------------
// Run / Once with ref target
// ---------------------------------------------------------------------------

func mkRefCommit(oid string, failing []string) *RefTarget {
	runs := make([]CheckRun, 0, len(failing))
	for _, name := range failing {
		runs = append(runs, CheckRun{Name: name, Conclusion: "FAILURE"})
	}
	rt := &RefTarget{}
	rt.Target.Oid = oid
	rt.Target.MessageHeadline = "headline"
	rt.Target.CheckSuites = SuiteNodes{Nodes: []CheckSuite{{App: AppInfo{Name: "CI"}, CheckRuns: RunNodes{Nodes: runs}}}}
	rt.Target.Authors = GitActorNodes{Nodes: []GitActor{{Name: "test", User: &struct {
		Login string `json:"login"`
	}{Login: "test"}}}}
	return rt
}

func scriptedRefAPI(responses []*RefTarget) *fakeAPI {
	call := 0
	return &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		idx := call
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		call++
		return assign(result, RefQueryResponse{Repository: struct {
			Ref *RefTarget `json:"ref"`
		}{Ref: responses[idx]}})
	}}
}

func TestRunRef_StreamsCIEvents(t *testing.T) {
	api := scriptedRefAPI([]*RefTarget{
		mkRefCommit("abc", nil),               // baseline, clean
		mkRefCommit("abc", []string{"build"}), // failing appears
		mkRefCommit("abc", nil),               // all green
		mkRefCommit("def", nil),               // new commit
	})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Ref: "main", Target: "ref", Host: "github.com"}

	ctx, cancel := context.WithCancel(context.Background())
	var got []Notification
	var firstPollSeen, failingSeen, greenSeen, commitSeen bool
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		// Check if we've seen enough events, then cancel.
		if firstPollSeen && failingSeen && greenSeen && commitSeen {
			cancel()
			return context.Canceled
		}
		return nil
	}

	err := Run(ctx, svc, opts, func(n Notification) {
		got = append(got, n)
		switch n.Type {
		case firstPollType:
			firstPollSeen = true
		case string(EventNewFailingChecks):
			failingSeen = true
		case string(EventCIAllGreen):
			greenSeen = true
		case string(EventNewCommit):
			commitSeen = true
		}
	})
	require.True(t, errors.Is(err, context.Canceled) || err == nil)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.True(t, failingSeen, "expected new-failing-checks")
	assert.True(t, greenSeen, "expected ci-all-green")
	assert.True(t, commitSeen, "expected new-commit")
}

func TestRunRef_FirstPollWithFailing_EmitsCIEvent(t *testing.T) {
	ref := mkRefCommit("abc1234", []string{"build"})
	api := scriptedRefAPI([]*RefTarget{ref, mkRefCommit("abc1234", nil)})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Ref: "main", Target: "ref", Host: "github.com"}

	var got []Notification
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled) || err == nil)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventNewFailingChecks))
	assert.Contains(t, types, string(EventNewCommit))
}

func TestOnceRef_EmitsCurrentActionable(t *testing.T) {
	ref := mkRefCommit("abc1234", []string{"build"})
	svc := &Service{API: scriptedRefAPI([]*RefTarget{ref})}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Ref: "main", Target: "ref", Host: "github.com"}

	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventNewFailingChecks))
	assert.Contains(t, types, string(EventNewCommit))
}

// ---------------------------------------------------------------------------
// Run / Once with issue target
// ---------------------------------------------------------------------------

func mkIssue(state string, comments []IssueComment) *IssueNode {
	return &IssueNode{State: state, Comments: IssueCommentNodes{Nodes: comments}}
}

func scriptedIssueAPI(responses []*IssueNode) *fakeAPI {
	call := 0
	return &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		idx := call
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		call++
		return assign(result, IssueQueryResponse{Repository: struct {
			Issue *IssueNode `json:"issue"`
		}{Issue: responses[idx]}})
	}}
}

func TestRunIssue_FirstPollWithComments_EmitsThem(t *testing.T) {
	issue := mkIssue("OPEN", []IssueComment{mkIssueComment("c1", "alice", "hey", false)})
	api := scriptedIssueAPI([]*IssueNode{issue})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Number: 42, Target: "issue", Host: "github.com"}
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventIssueNewComment))
}

func TestRunIssue_StreamsEvents(t *testing.T) {
	api := scriptedIssueAPI([]*IssueNode{
		mkIssue("OPEN", nil), // baseline
		mkIssue("OPEN", []IssueComment{mkIssueComment("c1", "alice", "hello", false)}), // new comment
		mkIssue("CLOSED", nil), // closed
	})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Number: 42, Target: "issue", Host: "github.com"}

	var got []Notification
	err := Run(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventIssueNewComment))
	assert.Contains(t, types, string(EventIssueClosed))
}

func TestRunIssue_AlreadyClosedAtStartup(t *testing.T) {
	api := scriptedIssueAPI([]*IssueNode{mkIssue("CLOSED", nil)})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Number: 42, Target: "issue", Host: "github.com"}

	var got []Notification
	err := Run(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)
	assert.Equal(t, []string{firstPollType, string(EventIssueClosed)}, typesOf(got))
}

func TestRun_EmitsExistingIssuesOnFirstPoll(t *testing.T) {
	pr := &PullRequest{
		State:     "OPEN",
		Mergeable: "CONFLICTING",
		Comments: CommentNodes{Nodes: []Comment{
			mkComment("c1", "alice", "please review", nil),
		}},
		ReviewThreads: ThreadNodes{Nodes: []ReviewThread{{
			ID:         "t1",
			IsResolved: false,
			Comments: CommentNodes{Nodes: []Comment{
				mkComment("tc1", "bob", "nit: rename", nil),
			}},
		}}},
		Commits: CommitNodes{Nodes: []Commit{mkCommit("abc1234def", []string{"build"})}},
	}
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	// First poll surfaces all pre-existing issues via diff against empty baseline.
	assert.Contains(t, types, string(EventConflict))
	assert.Contains(t, types, string(EventNewFailingChecks))
	assert.Contains(t, types, string(EventNewUnresolvedThreads))
	assert.Contains(t, types, string(EventNewGeneralComments))
	assert.Contains(t, types, string(EventNewCommit))
}

func TestRun_FailingChecksDetailWhenConflicted(t *testing.T) {
	// When a PR has BOTH merge conflicts AND failing CI, the failing-checks
	// notification's Detail field must guide the agent to resolve the conflict
	// first — preventing mistaken diagnosis of an Actions outage.
	pr := &PullRequest{
		State:     "OPEN",
		Mergeable: "CONFLICTING",
		Commits:   CommitNodes{Nodes: []Commit{mkCommit("abc1234def", []string{"build"})}},
	}
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	// Find the failing-checks notification.
	var failing *Notification
	for i := range got {
		if got[i].Type == string(EventNewFailingChecks) {
			failing = &got[i]
		}
	}
	require.NotNil(t, failing, "expected a new-failing-checks notification")
	assert.Contains(t, failing.Message, "build")
	assert.Contains(t, failing.Detail, "merge conflicts")
	assert.Contains(t, failing.Detail, "causing these CI failures")

	// Find the conflict notification — it should still fire independently.
	var conflict *Notification
	for i := range got {
		if got[i].Type == string(EventConflict) {
			conflict = &got[i]
		}
	}
	require.NotNil(t, conflict, "expected a conflict notification")
}

func TestRun_FailingChecksNoDetailWhenClean(t *testing.T) {
	// When a PR has failing CI but NO conflicts, the Detail field should NOT
	// contain conflict correlation — there is no conflict to correlate with.
	pr := &PullRequest{
		State:     "OPEN",
		Mergeable: "MERGEABLE",
		Commits:   CommitNodes{Nodes: []Commit{mkCommit("abc1234def", []string{"build"})}},
	}
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	var failing *Notification
	for i := range got {
		if got[i].Type == string(EventNewFailingChecks) {
			failing = &got[i]
		}
	}
	require.NotNil(t, failing, "expected a new-failing-checks notification")
	assert.NotContains(t, failing.Detail, "merge conflicts")
}

func TestRun_FirstPollCleanPR_OnlyFirstPoll(t *testing.T) {
	// PR with no issues: CI passing, no comments, no conflicts.
	// First poll should surface the new commit AND ci-all-green.
	svc := &Service{API: scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", nil),
		mkPR("MERGED", true, "aaaaaaa", nil),
	})}

	var got []Notification
	err := Run(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	assert.Equal(t, []string{firstPollType, string(EventNewCommit), string(EventCIAllGreen), string(EventMerged)}, typesOf(got))
}

func TestRun_FirstPollPendingCI_NoCIEvent(t *testing.T) {
	// CI is running (pending) but not failing → no CI event on first poll.
	pr := mkPR("OPEN", false, "aaaaaaa", nil)
	pr.Commits.Nodes[0].Commit.CheckSuites = SuiteNodes{Nodes: []CheckSuite{
		{Status: "IN_PROGRESS", App: AppInfo{Name: "CI"}},
	}}
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	// No CI events emitted (pending is not failing, and not all-green since prev had no work).
	assert.NotContains(t, types, string(EventNewFailingChecks))
	assert.NotContains(t, types, string(EventCIAllGreen))
}

func TestOnceIssue_EmitsCurrentActionable(t *testing.T) {
	issue := &IssueNode{
		State: "OPEN",
		Comments: IssueCommentNodes{Nodes: []IssueComment{
			mkIssueComment("c1", "alice", "please fix", false),
		}},
	}
	api := scriptedIssueAPI([]*IssueNode{issue})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Number: 42, Target: "issue", Host: "github.com"}

	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventIssueNewComment))
}

// ---------------------------------------------------------------------------
// Run / Once with workflow-run target
// ---------------------------------------------------------------------------

func scriptedRunAPI(runs []*WorkflowRun) *fakeAPI {
	call := 0
	return &fakeAPI{restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		_ = method
		_ = path
		_ = params
		_ = body
		idx := call
		if idx >= len(runs) {
			idx = len(runs) - 1
		}
		call++
		return assign(result, runs[idx])
	}}
}

func runRunOptions() RunOptions {
	return RunOptions{
		Identity: resolver.Identity{Owner: "octo", Repo: "demo", RunID: 30433642, Target: "run", Host: "github.com"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 60 * time.Second,
		Now:      func() time.Time { return time.Unix(0, 0).UTC() },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
}

func TestRunRun_StreamsUntilCompleted(t *testing.T) {
	svc := &Service{API: scriptedRunAPI([]*WorkflowRun{
		mkWorkflowRun("in_progress", ""), // baseline, running
		mkWorkflowRun("in_progress", ""), // no change
		mkWorkflowRun("completed", "success"),
	})}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Equal(t, string(EventRunCompleted), types[len(types)-1])

	var completed *Notification
	for i := range got {
		if got[i].Type == string(EventRunCompleted) {
			completed = &got[i]
		}
	}
	require.NotNil(t, completed)
	assert.Equal(t, "success", completed.Conclusion)
	assert.Equal(t, 30433642, completed.RunID)
	assert.Contains(t, completed.Message, "success")
	assert.NotEmpty(t, completed.PRUrl)
}

func TestRunRun_AlreadyCompletedAtStartup(t *testing.T) {
	svc := &Service{API: scriptedRunAPI([]*WorkflowRun{
		mkWorkflowRun("completed", "failure"),
	})}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventRunCompleted))
}

func TestRunRun_QueuedThenInProgressThenCompleted(t *testing.T) {
	svc := &Service{API: scriptedRunAPI([]*WorkflowRun{
		mkWorkflowRun("queued", ""),
		mkWorkflowRun("in_progress", ""),
		mkWorkflowRun("completed", "timed_out"),
	})}

	var got []Notification
	err := Run(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventRunInProgress))
	assert.Contains(t, types, string(EventRunCompleted))
	assert.Equal(t, string(EventRunCompleted), types[len(types)-1])
}

func TestRunRun_ContextCancelStops(t *testing.T) {
	svc := &Service{API: scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("in_progress", "")})}
	opts := runRunOptions()
	opts.Sleep = func(context.Context, time.Duration) error { return context.Canceled }

	var got []Notification
	err := Run(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	assert.ErrorIs(t, err, context.Canceled)
	require.NotEmpty(t, got)
	assert.Equal(t, firstPollType, got[0].Type)
}

func TestOnceRun_EmitsCurrentActionable(t *testing.T) {
	svc := &Service{API: scriptedRunAPI([]*WorkflowRun{mkWorkflowRun("completed", "success")})}

	var got []Notification
	err := Once(context.Background(), svc, runRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	assert.Contains(t, types, string(EventRunCompleted))
}
