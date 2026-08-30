package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkCommit builds a commit whose CI has finished: the suite is COMPLETED,
// and it concludes SUCCESS unless failing names are supplied. Mirroring a
// terminal state matters (see AGENTS.md): a suite with a blank status and
// blank conclusion does not exist in GitHub's API and would look "clean" for
// the wrong reason. This mirrors monitor.run_test.go's helper.
func mkCommit(oid string, failing []string) monitor.Commit {
	runs := make([]monitor.CheckRun, 0, len(failing))
	for _, name := range failing {
		runs = append(runs, monitor.CheckRun{Name: name, Conclusion: "FAILURE"})
	}
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "CI"}, CheckRuns: monitor.RunNodes{Nodes: runs}}
	if len(failing) > 0 {
		suite.Conclusion = "" // failing runs carry the failure; don't also flag the suite
	}
	return monitor.Commit{Commit: monitor.CommitDetails{
		Oid:             oid,
		MessageHeadline: "headline",
		CheckSuites:     monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
	}}
}

// prFixture builds an open PR with a single finished check suite. With no
// failing names the suite is COMPLETED/SUCCESS (CI green); with failing names
// the suite carries FAILURE check runs (CI red).
func prFixture(failing []string) *monitor.PullRequest {
	return &monitor.PullRequest{
		State:   "OPEN",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{mkCommit("aaaaaaa", failing)}},
	}
}

func testHubTarget() backend.Target {
	return backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7, Host: "github.com"}
}

func testHubOpts() backend.WatchOptions {
	return backend.WatchOptions{Interval: 60 * time.Second}
}

// collect reads event types from ch, waiting up to timeout for each event. It
// returns as soon as a read times out, so tests stay deterministic once a
// stable snapshot stops producing events.
func collect(ch <-chan backend.Update, timeout time.Duration) []string {
	var out []string
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, string(u.Event.Type))
		case <-time.After(timeout):
			return out
		}
	}
}

// swapFetcher swaps the hub's fetch function under its lock; used to advance
// the simulated PR state between refreshes.
func swapFetcher(h *Hub, f func(context.Context, resolver.Identity, monitor.QueryTier) (any, error)) {
	h.mu.Lock()
	h.fetch = f
	h.mu.Unlock()
}

// TestHub_SingleFetchFansOutToMultipleConsumers is the core acceptance test for
// issue #34: two consumers watching the same PR must share one fetch loop, not
// each poll GitHub independently. A single fetch must feed both consumers.
func TestHub_SingleFetchFansOutToMultipleConsumers(t *testing.T) {
	var fetches int64
	snap := prFixture([]string{"ci-build"})

	// A one-hour interval means the ticker never fires during the test; only
	// the poller's immediate first fetch runs, so fetches is deterministic.
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return snap, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	chA, cancelA := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelA)
	chB, cancelB := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelB)

	gotA := collect(chA, 200*time.Millisecond)
	gotB := collect(chB, 200*time.Millisecond)
	require.NotEmpty(t, gotA, "consumer A got nothing")
	require.NotEmpty(t, gotB, "consumer B got nothing")
	assert.Contains(t, gotA, "first-poll")
	assert.Contains(t, gotB, "first-poll")

	// Exactly one fetch served two consumers — the whole point of the hub.
	assert.Equal(t, int64(1), atomic.LoadInt64(&fetches),
		"two consumers must share a single fetch, not poll independently")
}

// TestHub_ConsumptionByOneDoesNotSuppressAnother is the requirement from #32:
// "Consumption by one consumer must not suppress delivery to another." A late
// subscriber must still see the current state as a first-poll even though an
// earlier consumer already consumed it, and a subsequent transition must reach
// both consumers against their own baselines.
func TestHub_ConsumptionByOneDoesNotSuppressAnother(t *testing.T) {
	// Start with a failing check; swap to green mid-test via RefreshPR.
	current := prFixture([]string{"ci-build"})
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return current, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Consumer A subscribes first and consumes the failing-check state.
	chA, cancelA := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelA)
	gotA := collect(chA, 200*time.Millisecond)
	require.Contains(t, gotA, "first-poll")
	require.Contains(t, gotA, "new-failing-checks",
		"A must see the failing check on first poll")

	// Consumer B subscribes after A has already consumed the same state.
	// B must still receive it as a first-poll — A's consumption must not
	// suppress delivery to B. This arrives from the poller's cached latest,
	// not a new fetch.
	var fetches int64
	swapFetcher(h, func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return current, nil
	})
	chB, cancelB := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelB)
	gotB := collect(chB, 200*time.Millisecond)
	require.Contains(t, gotB, "first-poll")
	require.Contains(t, gotB, "new-failing-checks",
		"B must see the failing check too; A consuming it must not suppress B")
	assert.Equal(t, int64(0), atomic.LoadInt64(&fetches),
		"B's first-poll must come from the cached snapshot, not a new fetch")

	// Now flip CI to green and trigger one shared refresh. Each consumer
	// diffs against its own baseline (the failing state), so both must see
	// ci-all-green — independently.
	current = prFixture(nil)
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))

	gotA2 := collect(chA, 200*time.Millisecond)
	gotB2 := collect(chB, 200*time.Millisecond)
	assert.Contains(t, gotA2, "ci-all-green", "A sees the transition to green")
	assert.Contains(t, gotB2, "ci-all-green", "B sees the transition to green")
	assert.Equal(t, int64(1), atomic.LoadInt64(&fetches),
		"the refresh did one fetch serving both consumers")
}

// TestHub_CancelRemovesConsumerAndStopsPollerWhenEmpty ensures the poller
// goroutine is torn down once its last consumer leaves, so a quiet repo does
// not leak a fetch loop.
func TestHub_CancelRemovesConsumerAndStopsPollerWhenEmpty(t *testing.T) {
	var fetches int64
	// Short interval so a leaked poller would keep fetching visibly.
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	_ = collect(ch, 200*time.Millisecond)
	first := atomic.LoadInt64(&fetches)
	require.GreaterOrEqual(t, first, int64(1), "poller fetched at least once")

	cancelSub()

	// After cancellation the poller is torn down. A fetch in flight when
	// stop was called may still complete, so allow one straggler — but a
	// leaked poller would keep fetching on its 5ms ticker (~16 in 80ms).
	time.Sleep(80 * time.Millisecond)
	second := atomic.LoadInt64(&fetches)
	assert.LessOrEqual(t, second-first, int64(1),
		"poller must stop fetching once its last consumer cancels")

	// The hub must not retain a poller with zero subscribers.
	h.mu.Lock()
	remaining := len(h.pollers)
	h.mu.Unlock()
	assert.Equal(t, 0, remaining, "hub must drop the empty poller")
}

// TestPoller_BacksOffWhenQuiet verifies the poller's idle backoff: on an
// unchanged PR the cadence doubles after three no-change polls, so a quiet
// PR watched through the shared daemon costs a fraction of the fetches a
// fixed ticker would. Before this change the poller polled at the base
// interval forever. The assertion is relative (later windows poll less than
// earlier ones) rather than absolute, so jittered delays cannot make it
// flaky.
func TestPoller_BacksOffWhenQuiet(t *testing.T) {
	var fetches int64
	snap := prFixture(nil) // never changes
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return snap, nil
	}, nil, 20*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 60*time.Millisecond) // drain the first polls; backoff begins

	first := atomic.LoadInt64(&fetches)
	time.Sleep(120 * time.Millisecond)
	mid := atomic.LoadInt64(&fetches)
	time.Sleep(120 * time.Millisecond)
	last := atomic.LoadInt64(&fetches)

	early := mid - first
	late := last - mid
	assert.GreaterOrEqual(t, early, int64(2), "the early window must still poll")
	assert.Less(t, late, early,
		"the cadence must slow as the PR stays quiet (backoff)")
}

// TestPoller_WakeResetsBackoffAndFetchesNow verifies that a wake (a fresh
// subscriber, or an explicit RefreshPR) drops any accumulated idle backoff
// and fetches immediately — a new consumer must not wait out a quiet-PR
// backoff before seeing current state.
func TestPoller_WakeResetsBackoffAndFetchesNow(t *testing.T) {
	var fetches int64
	var mu sync.Mutex
	current := prFixture(nil) // quiet: backoff will grow
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}, nil, 5*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond)

	// Let the backoff grow well past the base interval.
	time.Sleep(60 * time.Millisecond)
	before := atomic.LoadInt64(&fetches)

	// A change lands while the poller is backed off; the consumer must still
	// learn about it promptly. RefreshPR is the explicit wake path (the same
	// channel a new subscriber triggers).
	mu.Lock()
	current = prFixture([]string{"ci-build"})
	mu.Unlock()
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))

	got := collect(ch, 200*time.Millisecond)
	assert.Contains(t, got, "new-failing-checks",
		"a wake must fetch immediately even when the poller was backed off")
	assert.Greater(t, atomic.LoadInt64(&fetches), before,
		"the wake must produce at least one fetch")
}

// TestHub_ConcurrentSubscribersDoNotRace exercises the hub under concurrent
// subscribe/cancel churn to guard against data races (run with -race).
func TestHub_ConcurrentSubscribersDoNotRace(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, c := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
			_ = collect(ch, 100*time.Millisecond)
			c()
		}()
	}
	wg.Wait()
}

// TestPoller_BroadcastsDegradedOnFetchError verifies the loudness fix: a
// daemon poller whose fetch fails must reach subscribers with a degraded
// notification — a silently blind watcher would read as "all clear".
func TestPoller_BroadcastsDegradedOnFetchError(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, errors.New("gh api failed: exit status 1")
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	got := collect(ch, 200*time.Millisecond)
	require.NotEmpty(t, got, "the subscriber must receive something")
	var degraded bool
	for _, typ := range got {
		if typ == string(monitor.EventDegraded) {
			degraded = true
		}
	}
	assert.True(t, degraded, "a fetch error must reach the subscriber as a degraded notification")
}

// TestPoller_DegradedEpisodeEmitsOnce verifies hub-side episode dedup
// (issue #66): consecutive identical failed fetches are one episode, one
// broadcast — the shared poller must not spend one notification per poll on
// an outage, exactly like the in-process loops.
func TestPoller_DegradedEpisodeEmitsOnce(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, errors.New("gh api failed: exit status 1")
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	// The subscribe fetch fails and must degrade loudly, exactly once.
	waitDegraded(t, ch, "the first failed fetch must broadcast the degradation")

	// Two more identical failed polls via the explicit wake path: neither may
	// broadcast again.
	for i := 0; i < 2; i++ {
		require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
		time.Sleep(20 * time.Millisecond)
	}
	assertNoDegraded(t, ch, 200*time.Millisecond,
		"identical repeated failures are one episode: no further degraded broadcast")
}

// TestPoller_DegradedChangedMessageEmits verifies a changed error message is
// new information and re-broadcasts, while identical repeats stay silent.
func TestPoller_DegradedChangedMessageEmits(t *testing.T) {
	calls := 0
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		calls++
		return nil, fmt.Errorf("gh api failed: attempt %d", calls)
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	waitDegraded(t, ch, "the first failure must broadcast")
	for i := 0; i < 2; i++ {
		require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
		waitDegraded(t, ch, "each distinct error message must re-broadcast")
	}
	assertNoDegraded(t, ch, 100*time.Millisecond, "no broadcast without a further poll")
}

// TestPoller_DegradedRecoveryEmits verifies the recovery notice: after a
// degraded episode, the first successful fetch announces the surface is back
// and clears the episode, so a later failure degrades loudly again.
func TestPoller_DegradedRecoveryEmits(t *testing.T) {
	calls := 0
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("gh api failed: exit status 1")
		}
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	waitDegraded(t, ch, "the first failure must broadcast")

	// The next poll succeeds: a recovery notice must precede (or accompany)
	// the fresh snapshot.
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed while waiting for recovery")
			}
			if u.Event.Notice != "" && strings.Contains(u.Event.Notice, "recovered") {
				assert.Contains(t, u.Event.Notice, "graphql", "the recovery notice must name the surface")
				goto recovered
			}
		case <-deadline:
			t.Fatal("timed out waiting for the recovery notice")
		}
	}
recovered:
	// The episode is cleared: another success must not re-announce recovery.
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	assertNoDegraded(t, ch, 200*time.Millisecond,
		"a healthy poll after recovery must not re-announce")

	// And a fresh failure starts a new episode, broadcasting again.
	swapFetcher(h, func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, errors.New("gh api failed: again")
	})
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	waitDegraded(t, ch, "a failure after recovery is a new episode and must broadcast")
}

// waitDegraded reads updates until a fetch-error degraded broadcast arrives.
// Notices (broker health, tier shed, recovery) carry Notice text rather than
// DegradedMessage, so they do not satisfy the wait.
func waitDegraded(t *testing.T, ch <-chan backend.Update, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatalf("subscription closed: %s", msg)
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.DegradedMessage != "" {
				return
			}
		case <-deadline:
			t.Fatalf("timed out: %s", msg)
		}
	}
}

// assertNoDegraded drains ch for the timeout and fails if a fetch-error
// degraded broadcast arrives in the window.
func assertNoDegraded(t *testing.T, ch <-chan backend.Update, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.DegradedMessage != "" {
				t.Fatalf("unexpected degraded broadcast: %s", msg)
			}
		case <-deadline:
			return
		}
	}
}

// TestPoller_TierNoticeOnLowBudget verifies the poller sheds surfaces and
// says so loudly when the advisory GraphQL budget is low, and that the fetch
// receives the shed tier. A low remaining (2% of limit) maps to
// TierNoReviews: annotations and reviews are shed.
func TestPoller_TierNoticeOnLowBudget(t *testing.T) {
	var gotTier monitor.QueryTier
	svc := &monitor.Service{API: &rateLimitAPIStub{remaining: 100, limit: 5000}}
	budget := monitor.NewBudgetGuard(svc, 60*time.Second)

	h := New(func(ctx context.Context, _ resolver.Identity, tier monitor.QueryTier) (any, error) {
		gotTier = tier
		return prFixture(nil), nil
	}, nil, time.Hour, budget)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	got := collect(ch, 300*time.Millisecond)
	assert.Equal(t, monitor.TierNoReviews, gotTier,
		"the fetch must run at the shed tier for 2% remaining")
	assert.Contains(t, got, string(monitor.EventDegraded),
		"entering a shed tier must broadcast a degraded notice")
}

// TestPoller_FetchErrorNamesBlindSharedSurfaces verifies issue #98: a PR's
// check outcomes, head commit, and mergeability ride in the SAME GraphQL
// query as the shed-able surfaces, so when that query fails, those surfaces
// go blind too — and the degraded notice must say so. A notice that only
// says "graphql" lets a caller keep trusting CI signals that a degraded
// query can no longer deliver.
func TestPoller_FetchErrorNamesBlindSharedSurfaces(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, errors.New("gh api failed: exit status 1")
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	u := waitDegradedUpdate(t, ch, "a failed PR fetch must broadcast the degradation")
	require.NotEmpty(t, u.Event.DegradedSurfaces,
		"the notice must name every surface that went blind, not just the API name")
	assert.Contains(t, u.Event.DegradedSurfaces, "check outcomes",
		"checks ride the failed query; a caller must know they are not arriving")
	assert.Contains(t, u.Event.DegradedSurfaces, "head commit",
		"the head SHA rides the failed query; a caller must know it cannot see a new push")
	assert.Contains(t, u.Event.DegradedSurfaces, "mergeability")
}

// TestPoller_FetchErrorNamesBlindSharedSurfaces_NonPR verifies the blind-
// surface list is derived per kind: a ref watch carries checks only, so its
// degraded notice names checks — not comments or reviews it never fetched.
func TestPoller_FetchErrorNamesBlindSharedSurfaces_NonPR(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, errors.New("gh api failed: exit status 1")
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	refTarget := backend.Target{Kind: backend.KindRef, Owner: "o", Repo: "r", Ref: "refs/heads/main", Host: "github.com"}
	ch, cancelSub := h.Subscribe(ctx, refTarget, testHubOpts())
	t.Cleanup(cancelSub)

	u := waitDegradedUpdate(t, ch, "a failed ref fetch must broadcast the degradation")
	require.NotEmpty(t, u.Event.DegradedSurfaces)
	assert.Contains(t, u.Event.DegradedSurfaces, "check outcomes",
		"a ref watch exists to report check outcomes; the notice must say they are blind")
	assert.NotContains(t, u.Event.DegradedSurfaces, "comments",
		"ref queries carry no comments; naming them would be a lie in the other direction")
}

// TestPoller_TierNoticeStatesChecksStayWatched verifies the other half of
// issue #98: a shed-tier transition must state that PR status and check
// outcomes REMAIN watched, because "no longer watching comments" alone
// invites the reader to wonder whether checks went too.
func TestPoller_TierNoticeStatesChecksStayWatched(t *testing.T) {
	svc := &monitor.Service{API: &rateLimitAPIStub{remaining: 100, limit: 5000}}
	budget := monitor.NewBudgetGuard(svc, 60*time.Second)

	h := New(func(ctx context.Context, _ resolver.Identity, tier monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, budget)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed")
			}
			if u.Event.Type == monitor.EventDegraded && strings.Contains(u.Event.Notice, "no longer watching") {
				assert.Contains(t, u.Event.Notice, "PR status and check outcomes remain watched",
					"the shed notice must state what the tier still covers")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the tier-shed notice")
		}
	}
}

// waitDegradedUpdate is waitDegraded returning the whole update, for tests
// that need the structured degraded fields rather than just the event type.
func waitDegradedUpdate(t *testing.T, ch <-chan backend.Update, msg string) backend.Update {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatalf("subscription closed: %s", msg)
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.DegradedMessage != "" {
				return u
			}
		case <-deadline:
			t.Fatalf("timed out: %s", msg)
		}
	}
}

// TestPoller_RecoveryDeclaresTheGap verifies issue #99: the recovery notice
// after a degraded episode must declare the blind window with structured
// from/to timestamps, so a caller that knows it has a hole can fill it from
// REST. The cursor contract never replays — a cursor advances only on a
// successful fetch — so without the declaration, whatever happened during
// the window is missed forever and the recovery notice reads as an all-clear.
func TestPoller_RecoveryDeclaresTheGap(t *testing.T) {
	calls := 0
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("gh api failed: exit status 1")
		}
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	waitDegraded(t, ch, "the first failure must broadcast")

	// The next poll succeeds: the recovery notice must carry the gap window.
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed")
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.Notice != "" && strings.Contains(u.Event.Notice, "recovered") {
				assert.NotEmpty(t, u.Event.DegradedFrom,
					"the recovery notice must mark when the blind window opened")
				assert.NotEmpty(t, u.Event.DegradedTo,
					"the recovery notice must mark when the blind window closed")
				from, err := time.Parse(time.RFC3339, u.Event.DegradedFrom)
				require.NoError(t, err, "DegradedFrom must be RFC 3339")
				to, err := time.Parse(time.RFC3339, u.Event.DegradedTo)
				require.NoError(t, err, "DegradedTo must be RFC 3339")
				assert.True(t, !from.After(to), "the window must run forward: %s -> %s", from, to)
				assert.Contains(t, u.Event.Notice, "were not observed",
					"the recovery notice must say events inside the window were missed")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the recovery notice")
		}
	}
}

// TestPoller_RecoveryWithoutBlindWindowDeclaresNothing verifies a plain
// recovery (no degraded episode) carries no gap declaration: a broker-health
// notice or tier recovery must not read as a blind window.
func TestPoller_RecoveryWithoutBlindWindowDeclaresNothing(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	got := collect(ch, 200*time.Millisecond)
	assert.NotContains(t, got, string(monitor.EventDegraded),
		"a healthy watch emits no degraded events at all")
}

// TestHub_SetBrokerHealth_BroadcastsOnceOnTransition verifies the loud
// signal a broker-backed watcher must give per the module's "absence is not
// success" guidance: every health transition reaches every subscriber
// exactly once, and re-reporting the same state a second time (a duplicate
// OnState call, or the daemon re-affirming the same health) does not spam a
// second notice.
func TestHub_SetBrokerHealth_BroadcastsOnceOnTransition(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 100*time.Millisecond) // drain the first-poll

	h.SetBrokerHealth(true, "")
	gotHealthy := collect(ch, 100*time.Millisecond)
	require.Len(t, gotHealthy, 1, "a health transition must broadcast exactly one notice")
	assert.Equal(t, string(monitor.EventDegraded), gotHealthy[0])

	// Re-affirming the same state must not broadcast again.
	h.SetBrokerHealth(true, "")
	gotRepeat := collect(ch, 50*time.Millisecond)
	assert.Empty(t, gotRepeat, "reporting the same health twice must not double-broadcast")

	h.SetBrokerHealth(false, "connection lost: EOF")
	gotDegraded := collect(ch, 100*time.Millisecond)
	require.Len(t, gotDegraded, 1, "the degrade transition must also broadcast exactly one notice")
}

// TestHub_BrokerHealthy_ReflectsLastSetState verifies BrokerHealthy answers
// with whatever was last reported, defaulting to unhealthy (a hub that never
// had SetBrokerHealth called — no broker configured — must never claim
// healthy by default, since nextDelay's extended cap is gated on it).
func TestHub_BrokerHealthy_ReflectsLastSetState(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	healthy, cap := h.BrokerHealthy()
	assert.False(t, healthy, "a hub with no broker wired must default to unhealthy")
	assert.Zero(t, cap)

	h.SetBrokerIdleCap(30 * time.Minute)
	h.SetBrokerHealth(true, "")
	healthy, cap = h.BrokerHealthy()
	assert.True(t, healthy)
	assert.Equal(t, 30*time.Minute, cap)

	h.SetBrokerHealth(false, "boom")
	healthy, _ = h.BrokerHealthy()
	assert.False(t, healthy)
}

// TestPoller_NextDelayHonoursBrokerExtendedCap is the mechanism-level "prove
// it actually reduces polling" test: at the same noChange count (the same
// point in a quiet PR's backoff curve), nextDelay must return a strictly
// longer wait once the broker reports healthy with an extended idle cap
// configured than it does with no broker wired at all. A longer wait between
// fetches is, by construction, fewer fetches over any fixed window — the
// exact arithmetic (how many fewer, over a realistic window) is measured
// separately in internal/monitor's IdleIntervalCapped tests, which don't
// need a live poller and so can simulate a full 30-minute window without
// actually sleeping.
func TestPoller_NextDelayHonoursBrokerExtendedCap(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 60*time.Second, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond) // let the poller start and register

	h.mu.Lock()
	var p *poller
	for _, poller := range h.pollers {
		p = poller
	}
	h.mu.Unlock()
	require.NotNil(t, p, "the poller must exist once a consumer is subscribed")

	// Put the poller deep into its idle backoff — the state a genuinely
	// quiet PR reaches after a few minutes either way.
	p.mu.Lock()
	p.noChange = 20
	p.mu.Unlock()

	withoutBroker := p.nextDelay()
	assert.LessOrEqual(t, withoutBroker, 2*monitor.MaxIdleInterval,
		"with no broker configured, nextDelay must stay within the default ceiling (plus jitter)")

	h.SetBrokerIdleCap(2 * time.Hour)
	h.SetBrokerHealth(true, "")
	withBroker := p.nextDelay()

	assert.Greater(t, withBroker, withoutBroker,
		"a healthy broker with an extended idle cap must allow a longer idle wait than the default ceiling — that longer wait is the reduced polling")
}

// TestPoller_RevertsToNormalCadenceWhenBrokerDegrades is the "break it and
// confirm it falls back" test at the hub level (the broker package's own
// tests break the MQTT session itself; this proves the daemon's poller
// actually acts on that signal). A poller running under the broker's
// extended idle cap must, the moment the broker degrades, stop honouring
// that extension — nextDelay must read the normal ceiling on its very next
// call, not a stale extended one computed while the broker still looked up.
func TestPoller_RevertsToNormalCadenceWhenBrokerDegrades(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	h.SetBrokerIdleCap(time.Hour) // deliberately huge: a bug that keeps using it would starve the test
	h.SetBrokerHealth(true, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 60*time.Millisecond) // let noChange grow well past 3

	// Kill the broker transport.
	h.SetBrokerHealth(false, "connection lost: EOF")
	got := collect(ch, 60*time.Millisecond)
	require.Contains(t, got, string(monitor.EventDegraded), "the degrade must reach the subscriber as a loud notice")

	h.mu.Lock()
	var p *poller
	for _, poller := range h.pollers {
		p = poller
	}
	h.mu.Unlock()
	require.NotNil(t, p)

	d := p.nextDelay()
	assert.LessOrEqual(t, d, 2*monitor.MaxIdleInterval,
		"once degraded, nextDelay must fall back within the normal ceiling, not the broker-healthy extended one")
}

// rateLimitAPIStub serves GET /rate_limit over REST with the given GraphQL
// remaining/limit, mirroring the monitor package's test fixture.
type rateLimitAPIStub struct {
	remaining int
	limit     int
}

func (r *rateLimitAPIStub) REST(method, path string, params map[string]string, body interface{}, result interface{}) error {
	rl := monitor.RateLimitResponse{}
	rl.Resources.GraphQL.Remaining = r.remaining
	rl.Resources.GraphQL.Limit = r.limit
	rl.Resources.GraphQL.Reset = time.Now().Add(30 * time.Minute).Unix()
	data, err := json.Marshal(rl)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func (r *rateLimitAPIStub) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	return errors.New("unexpected GraphQL call")
}

// TestPoller_FetchErrorWithShedTierDoesNotPanic guards the error path once a
// tier has been selected (a regression the broadcast refactor could have
// introduced).
func TestPoller_ErrorAfterTierSelectionStillBroadcasts(t *testing.T) {
	// Error exactly once, between successful polls: a persistent error would
	// broadcast every 5ms and starve collect()'s timeout. One error is enough
	// to prove the degraded broadcast reaches the subscriber.
	calls := 0
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("boom")
		}
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	got := collect(ch, 100*time.Millisecond)
	var degraded bool
	for _, typ := range got {
		if typ == string(monitor.EventDegraded) {
			degraded = true
		}
	}
	assert.True(t, degraded, "an error after the first success must still degrade loudly")
}
