package hub

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/elecnix/gh-monitor/internal/testrec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetchObservation is one recorded fetch.
type fetchObservation struct {
	target  backend.Target
	tier    monitor.QueryTier
	changed bool
	at      time.Time
}

// fetchRecorder collects the hub's fetch observations. It is safe for
// concurrent use: several pollers fetch at once.
type fetchRecorder struct {
	seens testrec.Recorder[fetchObservation]
}

// record notes one fetch.
func (r *fetchRecorder) record(target backend.Target, tier monitor.QueryTier, changed bool, at time.Time) {
	r.seens.Add(fetchObservation{target: target, tier: tier, changed: changed, at: at})
}

// observations returns the recorded fetches, oldest first.
func (r *fetchRecorder) observations() []fetchObservation {
	return r.seens.All()
}

// TestHub_FetchObserverReportsTargetTierAndChange is the acceptance test for
// the hub half of issue #122: every fetch is reported with the target that
// was read, the tier it was read at, and whether the read changed anything.
//
// The "changed" answer is the one a log of delivered updates cannot give: a
// poll that finds nothing changed delivers no update at all, so the hub is
// the only place that knows it looked.
func TestHub_FetchObserverReportsTargetTierAndChange(t *testing.T) {
	rec := &fetchRecorder{}
	current := prFixture([]string{"ci-build"})
	h := New(func(ctx context.Context, _ resolver.Identity, tier monitor.QueryTier) (any, error) {
		return current, nil
	}, nil, time.Hour, nil, WithFetchObserver(rec.record))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	collect(ch, 200*time.Millisecond)

	// The first fetch has no previous observation to compare against, so it
	// counts as a change: the poller learned the target's state.
	seens := rec.observations()
	require.Len(t, seens, 1, "one fetch, one observation")
	assert.Equal(t, testHubTarget(), seens[0].target, "the observation names the target that was read")
	assert.Equal(t, monitor.TierFull, seens[0].tier, "the observation names the tier the read ran at")
	assert.True(t, seens[0].changed, "the first observation is a change: the poller learned the state")
	assert.False(t, seens[0].at.IsZero(), "the observation says when the read happened")

	// A second fetch of the same payload changes nothing. The hub must say
	// so: this is the line that shows how often the hub polls for nothing.
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	collect(ch, 200*time.Millisecond)

	seens = rec.observations()
	require.Len(t, seens, 2, "the second fetch is observed too")
	assert.False(t, seens[1].changed, "an unchanged payload is not a change")
	assert.Equal(t, seens[0].tier, seens[1].tier, "the tier is reported on every fetch")

	// A third fetch of a different payload changes something, and the
	// observation says so.
	current = prFixture(nil) // CI went green
	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	collect(ch, 200*time.Millisecond)

	seens = rec.observations()
	require.Len(t, seens, 3, "the third fetch is observed too")
	assert.True(t, seens[2].changed, "a changed payload is a change")
}

// TestHub_FetchObserverReportsTheShedTier proves the tier on the record is
// the tier the read actually ran at, not a constant: a read under a low
// GraphQL budget runs at a shed tier, and the record says which one — so a
// reader knows what the poll could and could not see.
func TestHub_FetchObserverReportsTheShedTier(t *testing.T) {
	rec := &fetchRecorder{}
	// 2% remaining maps to TierNoReviews: annotations and reviews are shed.
	svc := &monitor.Service{API: &rateLimitAPIStub{remaining: 100, limit: 5000}}
	budget := monitor.NewBudgetGuard(svc, 60*time.Second)

	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, budget, WithFetchObserver(rec.record))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	collect(ch, 300*time.Millisecond)

	seens := rec.observations()
	require.NotEmpty(t, seens, "the fetch is observed")
	assert.Equal(t, monitor.TierNoReviews, seens[0].tier,
		"the record names the shed tier the read ran at")
}

// TestHub_FetchObserverReportsTheTierTheReadRanAt proves the record names
// the tier the read actually ran at.
//
// Two cases make the difference visible. A one-shot read fetches at the full
// tier; the snapshot it distills carries a tier field of its own, whose zero
// value is the status tier, so reporting that field would understate the
// cost of every ordinary read. And a poll that hits a query-cost error
// retries one tier down: the record must name the tier that answered, not
// the one that failed.
func TestHub_FetchObserverReportsTheTierTheReadRanAt(t *testing.T) {
	// A one-shot read runs at the full tier.
	rec := &fetchRecorder{}
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, nil, WithFetchObserver(rec.record))
	t.Cleanup(h.Stop)

	out := h.Once(context.Background(), testHubTarget(), testHubOpts())
	for range out {
	}
	seens := rec.observations()
	require.Len(t, seens, 1, "the one-shot read is observed")
	assert.Equal(t, monitor.TierFull, seens[0].tier,
		"a one-shot read ran at the full tier, and the record must say so")

	// A poll whose full-tier fetch hits a query-cost error retries one tier
	// down, and the record names the tier that answered.
	rec2 := &fetchRecorder{}
	var calls int64
	h2 := New(func(ctx context.Context, _ resolver.Identity, tier monitor.QueryTier) (any, error) {
		if atomic.AddInt64(&calls, 1) == 1 {
			return nil, &ghcli.GraphQLError{Errors: []ghcli.GraphQLErrorEntry{
				{Message: "Something went wrong while executing your query. Your query exceeded the resource limits"},
			}}
		}
		return prFixture(nil), nil
	}, nil, time.Hour, nil, WithFetchObserver(rec2.record))
	t.Cleanup(h2.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h2.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	collect(ch, 300*time.Millisecond)

	seens = rec2.observations()
	require.Len(t, seens, 1, "the retried poll is observed")
	assert.Equal(t, monitor.TierNoAnnotations, seens[0].tier,
		"the record names the tier that answered, not the one that failed")
}

// TestHub_UpdatesCarryTheHubSource proves every update the hub delivers says
// it came from the hub (issue #122). A log that cannot tell a hub-served
// update from a sub-daemon-served one cannot answer "did this cost a poll?".
func TestHub_UpdatesCarryTheHubSource(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture([]string{"ci-build"}), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	var got []backend.Update
	deadline := time.After(2 * time.Second)
	for len(got) == 0 {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("the stream closed before any update arrived")
			}
			got = append(got, u)
		case <-deadline:
			t.Fatal("no update arrived")
		}
	}
	require.NotEmpty(t, got)
	for _, u := range got {
		assert.Equal(t, backend.SourceHub, u.Source, "every hub-delivered update names the hub as its source")
	}
}
