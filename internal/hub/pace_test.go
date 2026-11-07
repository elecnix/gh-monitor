package hub

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paceHub builds a hub on a fake clock whose budget guard reads store.
func paceHub(t *testing.T, store *ghcli.RateLimitStore, now *time.Time) *Hub {
	t.Helper()
	guard := monitor.NewBudgetGuard(nil, time.Minute)
	guard.UseObserved(store, "github.com")
	h := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Minute, guard)
	h.now = func() time.Time { return *now }
	t.Cleanup(h.Stop)
	return h
}

func addPollers(h *Hub, n int, cost float64) []*poller {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*poller
	for i := 1; i <= n; i++ {
		id := resolver.Identity{Owner: "o", Repo: "r", Number: i}
		p := newPoller(h, id, h.interval)
		p.started = true
		p.lastCost.Store(math.Float64bits(cost))
		h.pollers[p.key] = p
		out = append(out, p)
	}
	return out
}

func TestPace_AmpleBudgetLeavesTimerFetchesUngated(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := ghcli.NewRateLimitStore()
	store.Observe("", ghcli.GraphQLHeaders(5000, 5000, now.Add(time.Hour)), now)
	h := paceHub(t, store, &now)
	ps := addPollers(h, 3, 1)

	d, stretched := h.paceInterval()
	assert.False(t, stretched)
	assert.Equal(t, time.Minute, d)
	for _, p := range ps {
		ok, _ := h.grantTurn(p)
		assert.True(t, ok)
	}
}

func TestPace_ShortBudgetGrantsOldestFirst(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := ghcli.NewRateLimitStore()
	store.Observe("", ghcli.GraphQLHeaders(900, 5000, now.Add(time.Hour)), now)
	h := paceHub(t, store, &now)
	ps := addPollers(h, 3, 26)
	pace, stretched := h.paceInterval()
	require.True(t, stretched, "pace %v", pace)
	for i, p := range ps {
		p.lastRefresh.Store(now.Add(-time.Duration(i+1) * pace).UnixNano())
	}
	oldest := ps[2]

	ok, _ := h.grantTurn(ps[0])
	assert.False(t, ok, "a newer poller waits for an older one")
	ok, _ = h.grantTurn(oldest)
	assert.True(t, ok)
	ok, wait := h.grantTurn(ps[1])
	assert.False(t, ok, "the next slot opens one gap later")
	assert.Positive(t, wait)
}

func TestPace_IntervalGrowsStrictlyAsBudgetShrinks(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := ghcli.NewRateLimitStore()
	h := paceHub(t, store, &now)
	addPollers(h, 3, 26)
	prev := time.Duration(0)
	for i, remaining := range []int{5000, 1500, 900, 700, 600} {
		at := now.Add(time.Duration(i) * time.Second)
		store.Observe("", ghcli.GraphQLHeaders(remaining, 5000, now.Add(time.Hour)), at)
		d, _ := h.paceInterval()
		assert.Greater(t, d, prev, "remaining %d", remaining)
		prev = d
	}
	assert.Greater(t, prev, 10*time.Minute)
}

// TestPace_RestoredPollerThatNeverStartedDoesNotStarveTheOthers pins the fix
// for the Cite finding on #213. A poller restored from a handoff exists in
// h.pollers before any subscriber arrives, and its lastRefresh stays zero
// until its first fetch. Zero reads as 1970 to OldestDue, so it is the oldest
// due candidate forever and every started poller is denied while the pace is
// stretched.
func TestPace_RestoredPollerThatNeverStartedDoesNotStarveTheOthers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := ghcli.NewRateLimitStore()
	store.Observe("", ghcli.GraphQLHeaders(900, 5000, now.Add(time.Hour)), now)
	h := paceHub(t, store, &now)
	ps := addPollers(h, 2, 26)
	started, restored := ps[0], ps[1]
	started.started = true
	started.lastRefresh.Store(now.Add(-2 * time.Minute).UnixNano())
	restored.lastRefresh.Store(0)

	pace, stretched := h.paceInterval()
	require.True(t, stretched, "pace %v", pace)

	ok, wait := h.grantTurn(started)
	assert.True(t, ok, "a poller that never started must not hold the slot; waited %v", wait)
}
