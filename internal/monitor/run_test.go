package monitor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The in-process watch loops were deleted in the issue-#76 consolidation:
// watching goes through the shared poller's hub, which drives the same
// per-kind consumers these packages test directly (consumers_test.go) and the
// hub-level behaviour (internal/hub). What remains here are the exported
// cadence and cursor primitives both paths share.

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

func TestNextErrBackoff(t *testing.T) {
	base := 60 * time.Second
	assert.Equal(t, base, NextErrBackoff(0, base), "first failure backs off at the base interval")
	assert.Equal(t, 2*base, NextErrBackoff(base, base), "consecutive failures double")
	assert.Equal(t, maxErrBackoff, NextErrBackoff(10*maxErrBackoff, base), "capped at the error ceiling")
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

func TestClipRepoResponse(t *testing.T) {
	resp := mkRepoResponse(
		[]RepoPR{mkRepoPR(1, "old", "2025-01-01T00:00:00Z"), mkRepoPR(2, "new", "2025-02-01T00:00:00Z")},
		[]RepoIssue{mkRepoIssue(3, "old", "2024-12-31T00:00:00Z")},
	)
	filtered := ClipRepoResponse(resp, "2025-01-01T00:00:00Z")
	// Items created at or before the threshold are suppressed; later ones stay.
	assert.Len(t, filtered.Repository.PullRequests.Nodes, 1)
	assert.Equal(t, 2, filtered.Repository.PullRequests.Nodes[0].Number)
	assert.Empty(t, filtered.Repository.Issues.Nodes)
	// The original response is untouched (a copy, not an in-place filter).
	assert.Len(t, resp.Repository.PullRequests.Nodes, 2)
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
