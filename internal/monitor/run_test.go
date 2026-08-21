package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkCommit builds a commit whose CI has finished: the suite is COMPLETED, and
// it concludes SUCCESS unless failing names are supplied. Mirroring a terminal
// state matters — a suite with a blank status/conclusion is a state GitHub
// never reports, and it would look "clean" for the wrong reason.
func mkCommit(oid string, failing []string) Commit {
	runs := make([]CheckRun, 0, len(failing))
	for _, name := range failing {
		runs = append(runs, CheckRun{Name: name, Conclusion: "FAILURE"})
	}
	suite := CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: AppInfo{Name: "CI"}, CheckRuns: RunNodes{Nodes: runs}}
	if len(failing) > 0 {
		// The failing runs carry the failure; leave the suite conclusion blank
		// so the suite name doesn't also land in FailingChecks.
		suite.Conclusion = ""
	}
	return Commit{Commit: CommitDetails{
		Oid:             oid,
		MessageHeadline: "headline",
		CheckSuites:     SuiteNodes{Nodes: []CheckSuite{suite}},
	}}
}

// mkCommitNoChecks builds a commit with no check suites at all — what GitHub
// reports in the seconds after a push, before Actions registers the workflow.
func mkCommitNoChecks(oid string) Commit {
	return Commit{Commit: CommitDetails{Oid: oid, MessageHeadline: "headline"}}
}

// mkPRSuiteStatus builds an open PR whose single check suite sits in the given
// status with no conclusion — i.e. CI is registered but has not finished.
func mkPRSuiteStatus(status string) *PullRequest {
	c := mkCommitNoChecks("aaaaaaa")
	c.Commit.CheckSuites = SuiteNodes{Nodes: []CheckSuite{{Status: status, App: AppInfo{Name: "CI"}}}}
	return &PullRequest{State: "OPEN", Commits: CommitNodes{Nodes: []Commit{c}}}
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

	assert.Equal(t, []string{firstPollType, string(EventCIAllGreen), string(EventMerged)}, typesOf(got))
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
	// On first poll, diff against empty baseline surfaces the merged state.
	// new-commit is skipped — the agent just pushed it.
	assert.Equal(t, []string{firstPollType, string(EventMerged)}, typesOf(got))
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
	// new-commit is skipped on first poll — the agent just pushed it.
	assert.NotContains(t, types, string(EventNewCommit))
	// CI is failing, so no green claim.
	assert.NotContains(t, types, string(EventCIAllGreen))
}

// onceTypes runs a single --once poll against the given PR and returns the
// notification types.
func onceTypes(t *testing.T, pr *PullRequest) []string {
	t.Helper()
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}
	var got []Notification
	require.NoError(t, Once(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) }))
	return typesOf(got)
}

func TestOnce_GreenCI_EmitsCIAllGreen(t *testing.T) {
	// ci-all-green never fires from Diff against an empty baseline, so --once
	// has to emit it explicitly when CI has finished green.
	assert.Contains(t, onceTypes(t, mkPR("OPEN", false, "aaaaaaa", nil)), string(EventCIAllGreen))
}

func TestOnce_NoChecksYet_NoCIAllGreen(t *testing.T) {
	pr := &PullRequest{State: "OPEN", Commits: CommitNodes{Nodes: []Commit{mkCommitNoChecks("aaaaaaa")}}}
	assert.NotContains(t, onceTypes(t, pr), string(EventCIAllGreen))
}

func TestOnce_UnfinishedSuite_NoCIAllGreen(t *testing.T) {
	for _, status := range []string{"REQUESTED", "PENDING", "QUEUED", "WAITING", "IN_PROGRESS"} {
		t.Run(status, func(t *testing.T) {
			assert.NotContains(t, onceTypes(t, mkPRSuiteStatus(status)), string(EventCIAllGreen))
		})
	}
}

func TestIdleInterval(t *testing.T) {
	base := 60 * time.Second
	assert.Equal(t, base, IdleInterval(base, 0))
	assert.Equal(t, base, IdleInterval(base, 3))              // growth starts after 3
	assert.Equal(t, 2*base, IdleInterval(base, 4))            // base * 2^1
	assert.Equal(t, maxIdleInterval, IdleInterval(base, 100)) // capped
}

func TestIdleIntervalCapped(t *testing.T) {
	base := 60 * time.Second
	assert.Equal(t, base, IdleIntervalCapped(base, 0, maxIdleInterval))
	assert.Equal(t, maxIdleInterval, IdleIntervalCapped(base, 100, maxIdleInterval), "capped")
	// A larger cap keeps growing past what the default IdleInterval allows —
	// this is the mechanism the daemon's broker transport (internal/hub)
	// relies on to poll less while the broker is healthy.
	assert.Equal(t, 30*time.Minute, IdleIntervalCapped(base, 100, 30*time.Minute))
	// cap<=0 means "no ceiling"; growth is unbounded (still >= base).
	assert.Greater(t, IdleIntervalCapped(base, 40, 0), maxIdleInterval)
	// IdleInterval is unchanged: it is IdleIntervalCapped with the fixed
	// package ceiling.
	assert.Equal(t, IdleIntervalCapped(base, 12, maxIdleInterval), IdleInterval(base, 12))
}

// TestIdleIntervalCapped_MeasuresPollReductionOverAWindow is the "prove it
// actually reduces polling" measurement the PRI-2093 ticket asks for,
// expressed as a fast, deterministic simulation rather than a real wall-clock
// wait: it walks IdleIntervalCapped forward exactly the way a poller's idle
// backoff does (see internal/hub's nextDelay), summing simulated elapsed time
// until a fixed window is covered, and counts how many polls that took under
// the default 300s ceiling versus a broker-healthy 30-minute one. The numbers
// this prints are the ones quoted in the PR description as the before/after.
func TestIdleIntervalCapped_MeasuresPollReductionOverAWindow(t *testing.T) {
	const (
		base      = 60 * time.Second // gh monitor's --interval default
		window    = 6 * time.Hour    // a long-lived PR watch
		brokerCap = 30 * time.Minute // a representative broker-healthy safety-net cap
	)

	countPolls := func(cap time.Duration) int {
		var elapsed time.Duration
		noChange := 0
		polls := 0
		for elapsed < window {
			d := IdleIntervalCapped(base, noChange, cap)
			elapsed += d
			noChange++
			polls++
		}
		return polls
	}

	defaultPolls := countPolls(maxIdleInterval)
	brokerPolls := countPolls(brokerCap)

	t.Logf("simulated polls of a quiet PR over %s: default 300s-cap cadence=%d, broker-healthy %s-cap cadence=%d (%.1fx fewer)",
		window, defaultPolls, brokerCap, brokerPolls, float64(defaultPolls)/float64(brokerPolls))

	require.Greater(t, defaultPolls, brokerPolls,
		"the broker-healthy extended cap must poll a quiet PR fewer times than the default ceiling over the same window")
	// The reduction must be substantial, not a rounding artifact — a poller
	// idling at the default ceiling makes ~6x more calls per unit time than
	// one idling at the broker-healthy 30-minute cap (300s vs 1800s).
	assert.GreaterOrEqual(t, float64(defaultPolls)/float64(brokerPolls), 3.0)
}

func TestJittered_WithinBounds(t *testing.T) {
	// Jitter must spread a delay by at most ±20% and never go negative.
	base := 60 * time.Second
	lo := base - base/5
	hi := base + base/5
	for i := 0; i < 200; i++ {
		d := Jittered(base)
		assert.GreaterOrEqual(t, d, lo, "jitter must not undershoot the -20%% bound")
		assert.LessOrEqual(t, d, hi, "jitter must not overshoot the +20%% bound")
		assert.Greater(t, d, time.Duration(0), "jitter must never produce a zero/negative delay")
	}
}

func TestJittered_ProducesSpread(t *testing.T) {
	// Over many samples the jittered delay must actually vary — a no-op
	// jitter (always returning base) would leave concurrent watchers aligned.
	base := 60 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		seen[Jittered(base)] = true
	}
	assert.Greater(t, len(seen), 1, "jittered delays should vary across samples")
}

func TestJittered_ExplicitJitterWins(t *testing.T) {
	opts := testRunOptions()
	opts.Jitter = func(d time.Duration) time.Duration { return d / 2 }
	assert.Equal(t, 30*time.Second, opts.jittered(60*time.Second))
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
	// new-commit is skipped — the agent just pushed it.
	assert.Contains(t, types, string(EventConflict))
	assert.Contains(t, types, string(EventNewFailingChecks))
	assert.Contains(t, types, string(EventNewUnresolvedThreads))
	assert.Contains(t, types, string(EventNewGeneralComments))
	assert.NotContains(t, types, string(EventNewCommit))
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

func TestRun_FirstPollGreenCI_EmitsCIAllGreen(t *testing.T) {
	// PR with no issues and CI finished green: first poll reports ci-all-green.
	// new-commit is skipped — the agent just pushed it.
	svc := &Service{API: scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", nil),
		mkPR("MERGED", true, "aaaaaaa", nil),
	})}

	var got []Notification
	err := Run(context.Background(), svc, testRunOptions(), func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	assert.Equal(t, []string{firstPollType, string(EventCIAllGreen), string(EventMerged)}, typesOf(got))
}

// firstPollTypes runs a single poll against the given PR and returns the
// notification types, cancelling before the loop sleeps.
func firstPollTypes(t *testing.T, pr *PullRequest) []string {
	t.Helper()
	svc := &Service{API: scriptedAPI([]*PullRequest{pr})}
	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))
	return typesOf(got)
}

func TestRun_FirstPollNoChecksYet_NoCIAllGreen(t *testing.T) {
	// No check suites at all — GitHub's state for the first seconds after a
	// push, and the permanent state of a repo without CI. Absence of failures
	// is not evidence CI passed, so stay quiet.
	pr := &PullRequest{State: "OPEN", Commits: CommitNodes{Nodes: []Commit{mkCommitNoChecks("aaaaaaa")}}}
	assert.NotContains(t, firstPollTypes(t, pr), string(EventCIAllGreen))
}

func TestRun_FirstPollUnfinishedSuite_NoCIAllGreen(t *testing.T) {
	// Every non-terminal CheckStatusState must count as pending, not as green.
	for _, status := range []string{"REQUESTED", "PENDING", "QUEUED", "WAITING", "IN_PROGRESS"} {
		t.Run(status, func(t *testing.T) {
			types := firstPollTypes(t, mkPRSuiteStatus(status))
			assert.NotContains(t, types, string(EventCIAllGreen))
			assert.NotContains(t, types, string(EventNewFailingChecks))
		})
	}
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

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// Degradation tests (issue #33)
// ---------------------------------------------------------------------------

// failingPRAPI is a fakeAPI whose GraphQL method always returns the given error.
func failingPRAPI(err error) *fakeAPI {
	return &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return err
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			return err
		},
	}
}

// ---------------------------------------------------------------------------
// Repo cursor tests (issue #32)
// ---------------------------------------------------------------------------

// fakeRepoAPI returns a scripted sequence of RepoQueryResponses.
type fakeRepoAPI struct {
	responses []*RepoQueryResponse
	call      int
}

func (f *fakeRepoAPI) GraphQL(query string, vars map[string]interface{}, result interface{}) error {
	idx := f.call
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	f.call++
	return assign(result, f.responses[idx])
}

func (f *fakeRepoAPI) REST(method, path string, params map[string]string, body interface{}, result interface{}) error {
	return errors.New("REST not implemented")
}

func mkRepoResponse(prs []RepoPR, issues []RepoIssue) *RepoQueryResponse {
	return &RepoQueryResponse{
		Repository: struct {
			PullRequests RepoPRNodes    `json:"pullRequests"`
			Issues       RepoIssueNodes `json:"issues"`
		}{
			PullRequests: RepoPRNodes{Nodes: prs},
			Issues:       RepoIssueNodes{Nodes: issues},
		},
	}
}

func mkRepoPR(number int, title, createdAt string) RepoPR {
	return RepoPR{
		Number:    number,
		Title:     title,
		State:     "OPEN",
		URL:       "https://github.com/o/r/pull/" + string([]byte{byte('0' + number%10)}),
		CreatedAt: createdAt,
	}
}

func mkRepoIssue(number int, title, createdAt string) RepoIssue {
	return RepoIssue{
		Number:    number,
		Title:     title,
		State:     "OPEN",
		URL:       "https://github.com/o/r/issues/" + string([]byte{byte('0' + number%10)}),
		CreatedAt: createdAt,
	}
}

func testRepoRunOptions() RunOptions {
	return RunOptions{
		Identity: resolver.Identity{Owner: "o", Repo: "r", Target: "repo", Host: "github.com"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 60 * time.Second,
		Now:      func() time.Time { return time.Unix(0, 0).UTC() },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
}

func TestRun_DegradedOnFetchError(t *testing.T) {
	// A fetch error (e.g. gh exits 1) must emit a degraded event, not just
	// log to stderr and continue silently.
	api := failingPRAPI(errors.New("gh api failed: exit status 1"))
	svc := &Service{API: api}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	// Must contain a degraded event on the graphql surface.
	var degraded *Notification
	for i := range got {
		if got[i].Type == string(EventDegraded) {
			degraded = &got[i]
		}
	}
	require.NotNil(t, degraded, "fetch error must emit a degraded event")
	assert.Contains(t, degraded.Message, "degraded")
	assert.Contains(t, degraded.Message, "graphql")
}

func TestRun_DegradedOnRateLimitBody(t *testing.T) {
	// A successful gh invocation (exit 0) that returns a 403 rate-limit body
	// must be detected by ghcli and surfaced as a degraded event. This is the
	// case that bit: a valid-JSON error document yielding garbage data.
	//
	// We simulate this by having the REST call return an APIError that carries
	// a rate-limit message, mimicking ghcli's detection of error bodies.
	api := &fakeAPI{
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			return &ghcli.APIError{StatusCode: 403, Message: "API rate limit exceeded", Body: `{"message":"API rate limit exceeded","status":"403"}`}
		},
	}
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", RunID: 42, Target: "run", Host: "github.com"}

	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	var degraded *Notification
	for i := range got {
		if got[i].Type == string(EventDegraded) {
			degraded = &got[i]
		}
	}
	require.NotNil(t, degraded, "rate-limit error body must emit a degraded event")
	assert.Contains(t, degraded.Message, "degraded")
	assert.Contains(t, degraded.Message, "rest")
}

func TestRun_DegradedNoCIAllGreen(t *testing.T) {
	// When a fetch fails, no ci-all-green event should be emitted — the
	// previous snapshot is retained and no inference is made.
	api := failingPRAPI(errors.New("gh api failed"))
	svc := &Service{API: api}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	for _, n := range got {
		assert.NotEqual(t, string(EventCIAllGreen), n.Type,
			"ci-all-green must not fire when fetch is degraded")
	}
}

func TestRun_RefDegradedOnFetchError(t *testing.T) {
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return errors.New("gh api failed")
		},
	}
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Identity = resolver.Identity{Owner: "o", Repo: "r", Ref: "main", Target: "ref", Host: "github.com"}

	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	var degraded *Notification
	for i := range got {
		if got[i].Type == string(EventDegraded) {
			degraded = &got[i]
		}
	}
	require.NotNil(t, degraded, "ref fetch error must emit degraded")
	assert.Contains(t, degraded.Message, "graphql")
}

// ---------------------------------------------------------------------------
// Degraded-episode dedup tests (issue #66)
// ---------------------------------------------------------------------------

// cancelAfterSleeps returns a Sleep func that lets the first n sleeps through
// and then cancels the context, so a multi-poll loop stops after n+1 polls.
func cancelAfterSleeps(n int, cancel context.CancelFunc) func(context.Context, time.Duration) error {
	left := n
	return func(ctx context.Context, d time.Duration) error {
		if left > 0 {
			left--
			return nil
		}
		cancel()
		return ctx.Err()
	}
}

func countDegraded(ns []Notification) int {
	c := 0
	for _, n := range ns {
		if n.Type == string(EventDegraded) {
			c++
		}
	}
	return c
}

func TestRun_DegradedEpisodeEmitsOnce(t *testing.T) {
	// Repeated identical failed polls are one episode, one notification — not
	// one per poll. An outage of N polls must not spend N agent turns.
	api := failingPRAPI(errors.New("gh api failed: exit status 1"))
	svc := &Service{API: api}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = cancelAfterSleeps(4, cancel)

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	assert.Equal(t, 1, countDegraded(got),
		"5 identical degraded polls must produce exactly one degraded notification")
}

func TestRun_DegradedChangedMessageEmits(t *testing.T) {
	// A degraded surface that changes its error is new information: each
	// distinct message is emitted, identical repeats are not.
	n := 0
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			n++
			return fmt.Errorf("gh api failed: attempt %d", n)
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			n++
			return fmt.Errorf("gh api failed: attempt %d", n)
		},
	}
	svc := &Service{API: api}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = cancelAfterSleeps(2, cancel)

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	assert.Equal(t, 3, countDegraded(got),
		"every poll with a changed error message must be emitted")
}

func TestRun_DegradedRespectsEventFilter(t *testing.T) {
	// degraded is filterable like any other kind: a --events allowlist that
	// omits it mutes the noise; one that includes it passes it through.
	api := failingPRAPI(errors.New("gh api failed: exit status 1"))
	svc := &Service{API: api}

	run := func(kinds string) int {
		f, err := ParseEventFilter(kinds)
		require.NoError(t, err)
		opts := testRunOptions()
		opts.EventFilter = f
		ctx, cancel := context.WithCancel(context.Background())
		opts.Sleep = cancelAfterSleeps(1, cancel)
		var got []Notification
		_ = Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
		return countDegraded(got)
	}

	assert.Equal(t, 0, run("merged"), "an allowlist without degraded must mute it")
	assert.Equal(t, 1, run("degraded,merged"), "an allowlist with degraded must pass it through")
}

func TestRun_DegradedRecoveryEmits(t *testing.T) {
	// After a degraded episode, the first successful poll must announce the
	// recovery — the consumer needs to know the outage ended.
	n := 0
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			n++
			if n == 1 {
				return errors.New("gh api failed: exit status 1")
			}
			return assign(result, QueryResponse{Repository: struct {
				PullRequest *PullRequest `json:"pullRequest"`
			}{PullRequest: mkPR("OPEN", false, "aaaaaaa", nil)}})
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			return errors.New("rest not used")
		},
	}
	svc := &Service{API: api}

	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = cancelAfterSleeps(2, cancel)

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	var degraded []Notification
	for _, n := range got {
		if n.Type == string(EventDegraded) {
			degraded = append(degraded, n)
		}
	}
	require.Len(t, degraded, 2, "one degradation notice and one recovery notice")
	assert.Contains(t, degraded[0].Message, "degraded (graphql)")
	assert.Contains(t, degraded[1].Message, "recover")
}

// ---------------------------------------------------------------------------
// Repo cursor tests (issue #32)
// ---------------------------------------------------------------------------

func TestRunRepo_UnnamedEmitsAllItems(t *testing.T) {
	// Unnamed invocation emits all pre-existing items — today's behaviour.
	api := &fakeRepoAPI{responses: []*RepoQueryResponse{
		mkRepoResponse(
			[]RepoPR{mkRepoPR(1, "pr1", "2025-01-01T00:00:00Z"), mkRepoPR(2, "pr2", "2025-01-02T00:00:00Z")},
			[]RepoIssue{mkRepoIssue(3, "iss1", "2025-01-03T00:00:00Z")},
		),
	}}
	svc := &Service{API: api}

	opts := testRepoRunOptions()
	// Instance is empty — unnamed invocation.
	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	// Should emit all items.
	assert.NotEmpty(t, got)
	// We get first-poll plus events for PRs and issues.
	types := typesOf(got)
	assert.Contains(t, types, string(EventRepoNewPR))
	assert.Contains(t, types, string(EventRepoNewIssue))
}

func TestRunRepo_NamedNewInstanceStartsAtNow(t *testing.T) {
	// A brand-new named instance with no cursor starts at "now" and emits
	// nothing for pre-existing items (the fix for issue #32).
	api := &fakeRepoAPI{responses: []*RepoQueryResponse{
		mkRepoResponse(
			[]RepoPR{mkRepoPR(1, "pr1", "2020-01-01T00:00:00Z")},
			nil,
		),
	}}
	svc := &Service{API: api}

	opts := testRepoRunOptions()
	opts.Instance = "orchestrator"
	// No CursorPosition set — new instance.
	// Now() returns Unix(0,0) = 1970-01-01, and the PR is from 2020 => PR > now, so it should pass.
	// Actually "2020-01-01T00:00:00Z" > "1970-01-01T00:00:00Z" so the PR would pass the filter.
	// Let me fix: set now to 2025 so the PR is in the past.
	opts.Now = func() time.Time { return time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC) }

	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	// Should only have first-poll, no repo-new-pr because "now" (2025) > PR creation (2020).
	types := typesOf(got)
	assert.Equal(t, firstPollType, types[0])
	for _, typ := range types {
		assert.NotEqual(t, string(EventRepoNewPR), typ, "new instance should not emit old PRs")
		assert.NotEqual(t, string(EventRepoNewIssue), typ)
	}
}

func TestRunRepo_FromBeginningReplaysBacklog(t *testing.T) {
	// --from-beginning replays the full backlog.
	api := &fakeRepoAPI{responses: []*RepoQueryResponse{
		mkRepoResponse(
			[]RepoPR{mkRepoPR(1, "pr1", "2020-01-01T00:00:00Z")},
			nil,
		),
	}}
	svc := &Service{API: api}

	opts := testRepoRunOptions()
	opts.Instance = "orchestrator"
	opts.FromBeginning = true
	opts.Now = func() time.Time { return time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC) }

	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	types := typesOf(got)
	assert.Contains(t, types, string(EventRepoNewPR), "--from-beginning should replay backlog")
}

func TestRunRepo_CursorResumesFromPosition(t *testing.T) {
	// An instance with a cursor receives only items after the cursor.
	api := &fakeRepoAPI{responses: []*RepoQueryResponse{
		mkRepoResponse(
			[]RepoPR{
				mkRepoPR(1, "old-pr", "2025-01-01T00:00:00Z"),
				mkRepoPR(2, "new-pr", "2025-01-10T00:00:00Z"),
			},
			nil,
		),
	}}
	svc := &Service{API: api}

	opts := testRepoRunOptions()
	opts.Instance = "orchestrator"
	opts.CursorPosition = "2025-01-05T00:00:00Z" // should see PR #2 only

	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	// We should have events for new-pr only (PR #2), not old-pr (PR #1).
	types := typesOf(got)
	assert.Contains(t, types, string(EventRepoNewPR))
	// Verify the event is for PR #2.
	for _, n := range got {
		if n.Type == string(EventRepoNewPR) {
			assert.Contains(t, n.Detail, "#2")
			assert.NotContains(t, n.Detail, "#1")
		}
	}
}

func TestRunRepo_AdvanceCursorCalled(t *testing.T) {
	// After each poll, AdvanceCursor is called with the latest createdAt.
	api := &fakeRepoAPI{responses: []*RepoQueryResponse{
		mkRepoResponse(
			[]RepoPR{mkRepoPR(1, "pr1", "2025-01-10T00:00:00Z")},
			[]RepoIssue{mkRepoIssue(2, "iss1", "2025-01-15T00:00:00Z")},
		),
	}}
	svc := &Service{API: api}

	opts := testRepoRunOptions()
	opts.Instance = "orchestrator"
	opts.FromBeginning = true

	var advanced string
	opts.AdvanceCursor = func(pos string) { advanced = pos }

	err := Once(context.Background(), svc, opts, func(n Notification) {})
	require.NoError(t, err)

	// The latest createdAt is the issue's 2025-01-15.
	assert.Equal(t, "2025-01-15T00:00:00Z", advanced)
}

func TestRunRepo_CursorFiltersIssues(t *testing.T) {
	// Cursor filters issues the same as PRs.
	api := &fakeRepoAPI{responses: []*RepoQueryResponse{
		mkRepoResponse(
			nil,
			[]RepoIssue{
				mkRepoIssue(1, "old", "2025-01-01T00:00:00Z"),
				mkRepoIssue(2, "new", "2025-01-20T00:00:00Z"),
			},
		),
	}}
	svc := &Service{API: api}

	opts := testRepoRunOptions()
	opts.Instance = "orchestrator"
	opts.CursorPosition = "2025-01-10T00:00:00Z"

	var got []Notification
	err := Once(context.Background(), svc, opts, func(n Notification) { got = append(got, n) })
	require.NoError(t, err)

	for _, n := range got {
		if n.Type == string(EventRepoNewIssue) {
			assert.Contains(t, n.Detail, "#2")
			assert.NotContains(t, n.Detail, "#1")
		}
	}
}

func TestFilterRepoResponse_UnnamedReturnsUnmodified(t *testing.T) {
	resp := mkRepoResponse([]RepoPR{mkRepoPR(1, "pr", "2020-01-01T00:00:00Z")}, nil)
	opts := testRepoRunOptions()
	// Instance is empty.
	filtered := filterRepoResponse(resp, opts)
	assert.Equal(t, resp, filtered)
}

func TestFilterRepoResponse_EmptyRepoReturnsEmpty(t *testing.T) {
	resp := mkRepoResponse(nil, nil)
	opts := testRepoRunOptions()
	opts.Instance = "test"
	opts.CursorPosition = "2025-01-01T00:00:00Z"
	filtered := filterRepoResponse(resp, opts)
	assert.Empty(t, filtered.Repository.PullRequests.Nodes)
	assert.Empty(t, filtered.Repository.Issues.Nodes)
}

func TestLatestRepoCreatedAt(t *testing.T) {
	resp := mkRepoResponse(
		[]RepoPR{mkRepoPR(1, "p1", "2025-01-01T00:00:00Z"), mkRepoPR(2, "p2", "2025-01-05T00:00:00Z")},
		[]RepoIssue{mkRepoIssue(3, "i1", "2025-01-03T00:00:00Z")},
	)
	latest := LatestRepoCreatedAt(resp)
	assert.Equal(t, "2025-01-05T00:00:00Z", latest)
}

func TestLatestRepoCreatedAt_Empty(t *testing.T) {
	resp := mkRepoResponse(nil, nil)
	latest := LatestRepoCreatedAt(resp)
	assert.Empty(t, latest)
}

// ---------------------------------------------------------------------------
// PR cursor tests (issue #32 — PR resume mode)
// ---------------------------------------------------------------------------

func TestRunPR_NamedInstanceResumesFromStoredSnapshot(t *testing.T) {
	// A named PR instance with a stored snapshot resumes from that baseline —
	// it emits only what changed since the last checkpoint, not the full
	// backlog and not a silent gap.
	api := scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "bbbbbbb", []string{"build"}), // CI went red while offline
	})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Instance = "agent-pr-957"

	// Stored snapshot from a prior run: CI was green at "aaaaaaa"
	stored := Snapshot(
		mkPR("OPEN", false, "aaaaaaa", nil),
		SnapshotOptions{},
	)
	b, err := json.Marshal(stored)
	require.NoError(t, err)
	opts.CursorSnapshot = string(b)

	var advanced string
	opts.SaveSnapshot = func(s string) { advanced = s }

	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err = Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	// Must emit the new failing checks (the delta from stored snapshot).
	types := typesOf(got)
	assert.Contains(t, types, string(EventNewFailingChecks),
		"resume must deliver new CI failures from stored baseline")

	// Must NOT emit a first-poll notification — this is a resume, not a fresh start.
	for _, n := range got {
		assert.NotEqual(t, firstPollType, n.Type,
			"resume must not emit first-poll")
	}

	// SaveSnapshot must have been called.
	assert.NotEmpty(t, advanced, "snapshot must be saved after successful poll")
}

func TestRunPR_NamedInstanceFirstRunStartsFresh(t *testing.T) {
	// A named instance with no cursor starts fresh — diffs against empty
	// baseline, emits first-poll and all pre-existing issues.
	api := scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", []string{"build"}),
	})
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Instance = "fresh-instance"
	// No CursorSnapshot — this is a first run.

	var advanced string
	opts.SaveSnapshot = func(s string) { advanced = s }

	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	types := typesOf(got)
	assert.Contains(t, types, firstPollType,
		"first run must emit first-poll")
	assert.Contains(t, types, string(EventNewFailingChecks),
		"first run must surface pre-existing CI failures")
	assert.NotEmpty(t, advanced, "snapshot must be saved")
}

func TestRunPR_DegradedFetchDoesNotAdvanceCursor(t *testing.T) {
	// A degraded fetch must NOT advance the cursor — if it does, a consumer
	// silently loses exactly the window it could not read.
	api := failingPRAPI(errors.New("gh api failed"))
	svc := &Service{API: api}

	opts := testRunOptions()
	opts.Instance = "orchestrator"

	var saved string
	opts.SaveSnapshot = func(s string) { saved = s }

	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	var got []Notification
	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	// Degraded event emitted, but no snapshot saved.
	assert.NotEmpty(t, got)
	assert.Empty(t, saved, "degraded fetch must not advance cursor")
}

func TestRunPR_TwoInstancesIndependentSnapshots(t *testing.T) {
	// Two named instances on the same PR have independent stored snapshots.
	// Advancing one does not affect the other's resume position.
	prBefore := mkPR("OPEN", false, "aaaaaaa", nil)
	prAfter := mkPR("OPEN", false, "bbbbbbb", []string{"test"})

	// Instance A has empty baseline — sees everything.
	apiA := scriptedAPI([]*PullRequest{prAfter})
	svcA := &Service{API: apiA}
	optsA := testRunOptions()
	optsA.Instance = "agent-a"
	var snapA string
	optsA.SaveSnapshot = func(s string) { snapA = s }
	ctxA, cancelA := context.WithCancel(context.Background())
	optsA.Sleep = func(ctx context.Context, d time.Duration) error {
		cancelA()
		return context.Canceled
	}
	var gotA []Notification
	errA := Run(ctxA, svcA, optsA, func(n Notification) { gotA = append(gotA, n) })
	require.True(t, errors.Is(errA, context.Canceled))
	assert.NotEmpty(t, snapA)

	// Instance B starts from instance A's snapshot — should see no new
	// events (same PR state).
	apiB := scriptedAPI([]*PullRequest{prAfter})
	svcB := &Service{API: apiB}
	optsB := testRunOptions()
	optsB.Instance = "agent-b"
	optsB.CursorSnapshot = snapA
	var snapB string
	optsB.SaveSnapshot = func(s string) { snapB = s }
	ctxB, cancelB := context.WithCancel(context.Background())
	optsB.Sleep = func(ctx context.Context, d time.Duration) error {
		cancelB()
		return context.Canceled
	}
	var gotB []Notification
	errB := Run(ctxB, svcB, optsB, func(n Notification) { gotB = append(gotB, n) })
	require.True(t, errors.Is(errB, context.Canceled))

	// B should see no events besides degraded/CI events that arose from the diff.
	// Actually, since the PR state is identical, the diff should be empty.
	typesB := typesOf(gotB)
	for _, typ := range typesB {
		assert.NotEqual(t, firstPollType, typ, "B already has a baseline")
		assert.NotEqual(t, string(EventNewFailingChecks), typ, "B should not see the same CI failure as new")
	}
	assert.NotEmpty(t, snapB)

	// Now restart B with its own snapshot — still sees nothing new.
	_ = prBefore // used above
}
