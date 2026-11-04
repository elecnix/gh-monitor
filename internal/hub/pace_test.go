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

func TestPace_NextDelayStretchesWithShrinkingBudget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := ghcli.NewRateLimitStore()
	h := paceHub(t, store, &now)
	ps := addPollers(h, 3, 26)
	var prev time.Duration
	for _, remaining := range []int{5000, 1500, 900, 500, 0} {
		store.Observe("", ghcli.GraphQLHeaders(remaining, 5000, now.Add(time.Hour)), now.Add(time.Duration(remaining)*time.Millisecond))
		d, _ := h.paceInterval()
		assert.GreaterOrEqual(t, d, prev, "remaining %d", remaining)
		prev = d
		_ = ps
	}
}
