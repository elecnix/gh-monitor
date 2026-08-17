package hub

import (
	"context"
	"encoding/json"
	"errors"
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
func swapFetcher(h *Hub, f func(context.Context, resolver.Identity, monitor.QueryTier) (*monitor.PullRequest, error)) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	swapFetcher(h, func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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

// TestPoller_TierNoticeOnLowBudget verifies the poller sheds surfaces and
// says so loudly when the advisory GraphQL budget is low, and that the fetch
// receives the shed tier. A low remaining (2% of limit) maps to
// TierNoReviews: annotations and reviews are shed.
func TestPoller_TierNoticeOnLowBudget(t *testing.T) {
	var gotTier monitor.QueryTier
	svc := &monitor.Service{API: &rateLimitAPIStub{remaining: 100, limit: 5000}}
	budget := monitor.NewBudgetGuard(svc, 60*time.Second)

	h := New(func(ctx context.Context, _ resolver.Identity, tier monitor.QueryTier) (*monitor.PullRequest, error) {
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
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (*monitor.PullRequest, error) {
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
