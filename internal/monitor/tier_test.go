package monitor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTierSurfaces(t *testing.T) {
	cases := []struct {
		tier     QueryTier
		shed     []string
		comments bool
		reviews  bool
		anns     bool
	}{
		{TierFull, nil, true, true, true},
		{TierNoAnnotations, []string{"annotations"}, true, true, false},
		{TierNoReviews, []string{"annotations", "reviews", "review threads"}, true, false, false},
		{TierStatus, []string{"annotations", "reviews", "review threads", "comments"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.tier.String(), func(t *testing.T) {
			assert.Equal(t, tc.comments, tc.tier.HasComments())
			assert.Equal(t, tc.reviews, tc.tier.HasReviews())
			assert.Equal(t, tc.anns, tc.tier.HasAnnotations())
			assert.Equal(t, tc.shed, tc.tier.ShedSurfaces())
		})
	}
}

func TestTierForRemaining(t *testing.T) {
	// 5000-point hourly budget; thresholds 20% / 8% / 2%.
	assert.Equal(t, TierFull, TierForRemaining(5000, 5000))
	assert.Equal(t, TierFull, TierForRemaining(1000, 5000)) // exactly 20%
	assert.Equal(t, TierNoAnnotations, TierForRemaining(999, 5000))
	assert.Equal(t, TierNoAnnotations, TierForRemaining(400, 5000)) // exactly 8%
	assert.Equal(t, TierNoReviews, TierForRemaining(399, 5000))
	assert.Equal(t, TierNoReviews, TierForRemaining(100, 5000)) // exactly 2%
	assert.Equal(t, TierStatus, TierForRemaining(99, 5000))
	assert.Equal(t, TierStatus, TierForRemaining(0, 5000))
	// A broken limit must not panic or shed.
	assert.Equal(t, TierFull, TierForRemaining(100, 0))
}

func TestMonitorQuery_Tiers(t *testing.T) {
	full := MonitorQuery(TierFull)
	// The full query must contain every surface.
	for _, frag := range []string{"annotations(first: 50)", "reviewThreads(last: 25)", "reviews(last: 100)", "comments(last: 25)", "checkSuites(last: 50)", "state", "mergeable"} {
		assert.Contains(t, full, frag, "TierFull must contain %s", frag)
	}

	noAnns := MonitorQuery(TierNoAnnotations)
	assert.NotContains(t, noAnns, "annotations", "TierNoAnnotations must shed annotations")
	assert.Contains(t, noAnns, "reviewThreads(last: 25)", "TierNoAnnotations keeps review threads")
	assert.Contains(t, noAnns, "reviews(last: 100)", "TierNoAnnotations keeps reviews")
	assert.Contains(t, noAnns, "comments(last: 25)", "TierNoAnnotations keeps comments")
	assert.Contains(t, noAnns, "checkSuites(last: 50)", "checks are never shed")

	noReviews := MonitorQuery(TierNoReviews)
	assert.NotContains(t, noReviews, "reviewThreads", "TierNoReviews sheds review threads")
	assert.NotContains(t, noReviews, "reviews(", "TierNoReviews sheds reviews")
	assert.Contains(t, noReviews, "comments(last: 25)", "TierNoReviews keeps comments")
	assert.Contains(t, noReviews, "checkSuites(last: 50)", "checks are never shed")

	status := MonitorQuery(TierStatus)
	assert.NotContains(t, status, "comments(last: 25)", "TierStatus sheds comments")
	assert.NotContains(t, status, "reviewThreads", "TierStatus sheds review threads")
	assert.NotContains(t, status, "annotations", "TierStatus sheds annotations")
	assert.Contains(t, status, "checkSuites(last: 50)", "TierStatus keeps checks")
	assert.Contains(t, status, "mergeable", "TierStatus keeps PR status")
}

func TestMonitorQuery_Tiers_WellFormed(t *testing.T) {
	// Each tier's query must balance braces and end cleanly — a malformed
	// query would fail at runtime, not at build time.
	for _, tier := range []QueryTier{TierFull, TierNoAnnotations, TierNoReviews, TierStatus} {
		q := MonitorQuery(tier)
		opens := strings.Count(q, "{")
		closes := strings.Count(q, "}")
		assert.Equal(t, opens, closes, "%s: unbalanced braces", tier)
		assert.True(t, strings.HasSuffix(strings.TrimSpace(q), "}"), "%s: must end with a closing brace", tier)
	}
}

func TestCarryForwardShed(t *testing.T) {
	// A shed surface must keep its last-known value: dropping reviews must
	// not turn APPROVED into a dismissal, dropping annotations must not
	// erase them, and shedding comments/threads must not re-emit them.
	prev := &PRStatus{
		State:             "OPEN",
		ReviewDecision:    "APPROVED",
		ReviewAuthor:      "alice",
		UnresolvedThreads: []ThreadSummary{{ID: "t1"}},
		GeneralComments:   []GeneralComment{{ID: "c1"}},
		CheckAnnotations:  []AnnotationSummary{{CheckName: "ci", Title: "x"}},
	}

	t.Run("no-op when nothing shed", func(t *testing.T) {
		curr := &PRStatus{State: "OPEN"}
		CarryForwardShed(prev, curr)
		assert.Empty(t, curr.ReviewDecision, "nothing shed: snapshot stays as fetched")
	})

	t.Run("annotations carried", func(t *testing.T) {
		curr := &PRStatus{State: "OPEN", ShedSurfaces: []string{"annotations"}}
		CarryForwardShed(prev, curr)
		assert.Equal(t, prev.CheckAnnotations, curr.CheckAnnotations, "annotations must be carried forward")
	})

	t.Run("reviews carried", func(t *testing.T) {
		curr := &PRStatus{State: "OPEN", ShedSurfaces: []string{"reviews"}}
		CarryForwardShed(prev, curr)
		assert.Equal(t, "APPROVED", curr.ReviewDecision, "review decision must be carried forward")
		assert.Equal(t, "alice", curr.ReviewAuthor)
	})

	t.Run("threads and comments carried", func(t *testing.T) {
		curr := &PRStatus{State: "OPEN", ShedSurfaces: []string{"review threads", "comments"}}
		CarryForwardShed(prev, curr)
		assert.Equal(t, prev.UnresolvedThreads, curr.UnresolvedThreads)
		assert.Equal(t, prev.GeneralComments, curr.GeneralComments)
	})

	t.Run("nil prev is a no-op", func(t *testing.T) {
		curr := &PRStatus{State: "OPEN", ShedSurfaces: []string{"reviews"}}
		CarryForwardShed(nil, curr)
		assert.Empty(t, curr.ReviewDecision)
	})
}

func TestCarryForwardShed_PreventsFalseEvents(t *testing.T) {
	// The consumer-visible contract: diffing a shed snapshot against a full
	// one must NOT fire review-dismissed, ci-all-green, or re-emit threads.
	prev := &PRStatus{
		State:             "OPEN",
		ReviewDecision:    "APPROVED",
		ReviewAuthor:      "alice",
		UnresolvedThreads: []ThreadSummary{{ID: "t1"}},
		FailingChecks:     []string{"ci-build"},
	}
	curr := &PRStatus{State: "OPEN", FailingChecks: []string{"ci-build"}, ShedSurfaces: []string{"reviews", "review threads"}}
	CarryForwardShed(prev, curr)

	events := Diff(prev, curr)
	assert.Empty(t, events, "a shed snapshot must produce no false events")
}
