package monitor

import (
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var paceT0 = time.Unix(1_800_000_000, 0)

func paceReading(remaining int, at time.Time) ghcli.RateLimitReading {
	return ghcli.RateLimitReading{
		Resource: "graphql", Limit: 5000, Remaining: remaining,
		Reset: paceT0.Add(time.Hour), ObservedAt: at,
	}
}

const paceBase = 60 * time.Second

func TestPacer_AmpleBudgetKeepsBaseInterval(t *testing.T) {
	p := NewPacer(DefaultPaceConfig)
	r := paceReading(5000, paceT0)
	p.Observe(r)
	assert.Equal(t, paceBase, p.Interval(paceT0, r, paceBase, 5, 1))
}

func TestPacer_IntervalGrowsAsRemainingFalls(t *testing.T) {
	p := NewPacer(DefaultPaceConfig)
	var prev time.Duration
	for _, remaining := range []int{5000, 2000, 1000, 800, 600, 500, 400, 100, 0} {
		r := paceReading(remaining, paceT0)
		d := p.Interval(paceT0, r, paceBase, 20, 26)
		assert.GreaterOrEqual(t, d, prev, "remaining %d", remaining)
		prev = d
	}
	assert.Equal(t, time.Hour, prev, "no budget left backs off to the reset")
}

func TestPacer_SecondConsumerSlowsAnIdleDaemon(t *testing.T) {
	p := NewPacer(DefaultPaceConfig)
	r0 := paceReading(4000, paceT0)
	p.Observe(r0)
	before := p.Interval(paceT0, r0, paceBase, 10, 26)

	// This daemon spent nothing, yet 600 points vanished in a minute.
	r1 := paceReading(3400, paceT0.Add(time.Minute))
	p.Observe(r1)
	require.InDelta(t, 3.0, p.OtherRate(), 0.01, "0.3 x 10 points/s")
	after := p.Interval(paceT0.Add(time.Minute), r1, paceBase, 10, 26)
	assert.Greater(t, after, before)
}

func TestPacer_OwnSpendIsNotOtherConsumption(t *testing.T) {
	p := NewPacer(DefaultPaceConfig)
	p.Observe(paceReading(4000, paceT0))
	p.Spend(600)
	p.Observe(paceReading(3400, paceT0.Add(time.Minute)))
	assert.Zero(t, p.OtherRate())
}

func TestPacer_NewWindowResetsBaseline(t *testing.T) {
	p := NewPacer(DefaultPaceConfig)
	p.Observe(paceReading(10, paceT0))
	next := paceReading(5000, paceT0.Add(2*time.Hour))
	next.Reset = paceT0.Add(3 * time.Hour)
	p.Observe(next)
	assert.Zero(t, p.OtherRate())
}

func TestOldestDue_PicksLeastRecentlyRefreshed(t *testing.T) {
	now := paceT0.Add(10 * time.Minute)
	id, ok := OldestDue(now, []PollCandidate{
		{ID: 1, LastRefresh: now.Add(-3 * time.Minute), Interval: time.Minute},
		{ID: 2, LastRefresh: now.Add(-9 * time.Minute), Interval: time.Minute},
		{ID: 3, LastRefresh: now.Add(-30 * time.Second), Interval: time.Minute},
	})
	require.True(t, ok)
	assert.Equal(t, 2, id)

	_, ok = OldestDue(now, []PollCandidate{{ID: 3, LastRefresh: now, Interval: time.Minute}})
	assert.False(t, ok)
}

func TestPacer_TinyAvailableBudgetBacksOffToReset(t *testing.T) {
	p := NewPacer(DefaultPaceConfig)
	r := paceReading(501, paceT0)
	p.otherRate = 2.7775e-4 // leaves about 1e-4 points available
	assert.Equal(t, time.Hour, p.Interval(paceT0, r, paceBase, 20, 26))
}

// TestDefaultPaceConfig_FullTierCostMatchesTheQuery pins the full tier's
// cost to what its query actually spends. The full tier used to read
// annotations under 50 check suites of 50 runs, which GitHub bills once per
// parent: 26 points. It now reads annotation counts over REST and fetches
// bodies only for a run that changed, which measured one GraphQL point.
// A change that adds another nested connection to the query must raise this
// number, so the pacer does not plan for a poll that costs more than it thinks.
func TestDefaultPaceConfig_FullTierCostMatchesTheQuery(t *testing.T) {
	assert.Equal(t, 1.0, DefaultPaceConfig.PollCost(TierFull))
	assert.Equal(t, 1.0, DefaultPaceConfig.PollCost(TierStatus))
	assert.Contains(t, MonitorQuery(TierFull), "checkRuns(")
	assert.NotContains(t, MonitorQuery(TierFull), "annotations(")
}
