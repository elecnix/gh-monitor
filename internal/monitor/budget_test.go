package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rateLimitAPI returns a fake API whose REST endpoint serves rate_limit with
// the given GraphQL remaining/limit.
func rateLimitAPI(graphqlRemaining, graphqlLimit int) *fakeAPI {
	return &fakeAPI{
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			if path != "rate_limit" {
				return errors.New("unexpected REST path: " + path)
			}
			rl := RateLimitResponse{}
			rl.Resources.GraphQL.Remaining = graphqlRemaining
			rl.Resources.GraphQL.Limit = graphqlLimit
			rl.Resources.Core.Remaining = 4900
			rl.Resources.Core.Limit = 5000
			rl.Resources.Core.Reset = time.Now().Add(30 * time.Minute).Unix()
			rl.Resources.GraphQL.Reset = time.Now().Add(30 * time.Minute).Unix()
			return assign(result, rl)
		},
	}
}

func TestBudgetGuard_HealthyNoStretch(t *testing.T) {
	svc := &Service{API: rateLimitAPI(4900, 5000)}
	g := NewBudgetGuard(svc, 60*time.Second)

	st := g.Stretch(time.Now())
	assert.False(t, st.Low, "a healthy budget must not stretch")
	assert.Zero(t, st.Extra)
	assert.Equal(t, 4900, st.Remaining)
	assert.Equal(t, 5000, st.Limit)
	// No transition on a healthy first read — nothing to report.
	assert.False(t, st.Changed)
}

func TestBudgetGuard_LowStretches(t *testing.T) {
	svc := &Service{API: rateLimitAPI(300, 5000)} // 6% < 10% threshold
	g := NewBudgetGuard(svc, 60*time.Second)

	st := g.Stretch(time.Now())
	assert.True(t, st.Low, "budget below the threshold must stretch")
	assert.Greater(t, st.Extra, time.Duration(0), "stretch must add delay")
	// Threshold = 500; remaining 300 → deficit 0.4 → extra = 60s*3*0.4 = 72s.
	assert.Equal(t, 72*time.Second, st.Extra)
	assert.True(t, st.Changed, "entering the low state is a transition")
}

func TestBudgetGuard_ExhaustedStretchCapped(t *testing.T) {
	svc := &Service{API: rateLimitAPI(0, 5000)}
	g := NewBudgetGuard(svc, 60*time.Second)

	st := g.Stretch(time.Now())
	assert.True(t, st.Low)
	// deficit = 1 → 180s, within the 300s cap.
	assert.Equal(t, 180*time.Second, st.Extra)
}

func TestBudgetGuard_TransitionTracking(t *testing.T) {
	api := rateLimitAPI(300, 5000)
	svc := &Service{API: api}
	g := NewBudgetGuard(svc, 60*time.Second)
	now := time.Now()

	st := g.Stretch(now)
	require.True(t, st.Changed, "first low read must report the transition")

	st = g.Stretch(now)
	assert.False(t, st.Changed, "a second low read is not a transition")

	// Recovery: swap the served budget back to healthy and advance past the
	// refresh window.
	api.restFunc = func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		rl := RateLimitResponse{}
		rl.Resources.GraphQL.Remaining = 4900
		rl.Resources.GraphQL.Limit = 5000
		return assign(result, rl)
	}
	st = g.Stretch(now.Add(31 * time.Second))
	assert.False(t, st.Low)
	assert.True(t, st.Changed, "recovery must report the transition")
	assert.Zero(t, st.Extra)
}

func TestBudgetGuard_RefreshRateLimited(t *testing.T) {
	// The rate-limit endpoint is read at most once per checkEvery; between
	// refreshes the guard answers from the cache without another REST call.
	calls := 0
	api := &fakeAPI{
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			calls++
			rl := RateLimitResponse{}
			rl.Resources.GraphQL.Remaining = 300
			rl.Resources.GraphQL.Limit = 5000
			return assign(result, rl)
		},
	}
	svc := &Service{API: api}
	g := NewBudgetGuard(svc, 60*time.Second)
	now := time.Now()

	_, _, ok := g.GraphQLRemaining(now)
	require.True(t, ok)
	_, _, ok = g.GraphQLRemaining(now.Add(5 * time.Second))
	require.True(t, ok)
	assert.Equal(t, 1, calls, "a second read inside checkEvery must use the cache")
}

func TestBudgetGuard_BlindOnRateLimitError(t *testing.T) {
	// When the rate-limit endpoint itself cannot be read, the guard must not
	// guess: no stretch, no transition.
	api := &fakeAPI{restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		return errors.New("gh api failed")
	}}
	svc := &Service{API: api}
	g := NewBudgetGuard(svc, 60*time.Second)

	st := g.Stretch(time.Now())
	assert.False(t, st.Low)
	assert.Zero(t, st.Extra)
	assert.False(t, st.Changed)
}

func TestRun_BudgetLowEmitsNoticeAndStretches(t *testing.T) {
	// A watcher with a low GraphQL budget must stretch its cadence and emit a
	// loud degraded notice on entering the low state.
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, QueryResponse{Repository: struct {
				PullRequest *PullRequest `json:"pullRequest"`
			}{PullRequest: mkPR("OPEN", false, "aaaaaaa", []string{"ci-build"})}})
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			rl := RateLimitResponse{}
			rl.Resources.GraphQL.Remaining = 300
			rl.Resources.GraphQL.Limit = 5000
			rl.Resources.GraphQL.Reset = time.Now().Add(30 * time.Minute).Unix()
			return assign(result, rl)
		},
	}
	svc := &Service{API: api}

	var slept []time.Duration
	opts := testRunOptions()
	opts.Budget = NewBudgetGuard(svc, opts.Interval)
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) >= 2 {
			cancel()
			return context.Canceled
		}
		return nil
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
	require.NotNil(t, degraded, "entering the low budget state must emit a degraded notice")
	assert.Contains(t, degraded.Message, "budget low")
	assert.Contains(t, degraded.Message, "300/5000")
	// First poll: baseline; second poll: the stretch applies to the idle
	// sleep, which must exceed the plain base interval.
	require.GreaterOrEqual(t, len(slept), 1)
	assert.Greater(t, slept[0], 60*time.Second,
		"the idle sleep must be stretched beyond the base interval when the budget is low")
}

func TestRun_BudgetRecoveryEmitsNotice(t *testing.T) {
	// Recovering from the low state must emit a recovery notice, so a lifted
	// stretch is never silent.
	low := true
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, QueryResponse{Repository: struct {
				PullRequest *PullRequest `json:"pullRequest"`
			}{PullRequest: mkPR("OPEN", false, "aaaaaaa", []string{"ci-build"})}})
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			rl := RateLimitResponse{}
			if low {
				rl.Resources.GraphQL.Remaining = 300
			} else {
				rl.Resources.GraphQL.Remaining = 4900
			}
			rl.Resources.GraphQL.Limit = 5000
			rl.Resources.GraphQL.Reset = time.Now().Add(30 * time.Minute).Unix()
			return assign(result, rl)
		},
	}
	svc := &Service{API: api}

	var got []Notification
	opts := testRunOptions()
	opts.Budget = NewBudgetGuard(svc, opts.Interval)
	ctx, cancel := context.WithCancel(context.Background())
	cur := time.Unix(0, 0).UTC()
	opts.Now = func() time.Time { return cur }
	polls := 0
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		polls++
		// Advance past the guard's refresh window so the recovery becomes
		// visible on the next Stretch.
		cur = cur.Add(31 * time.Second)
		if polls == 2 {
			low = false
		}
		if polls >= 4 {
			cancel()
			return context.Canceled
		}
		return nil
	}

	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	var recovered *Notification
	var lowNotices int
	for i := range got {
		if got[i].Type == string(EventDegraded) {
			if got[i].Message != "" {
				if strings.Contains(got[i].Message, "budget low") {
					lowNotices++
				}
				if strings.Contains(got[i].Message, "budget recovered") {
					recovered = &got[i]
				}
			}
		}
	}
	require.NotNil(t, recovered, "recovering from the low state must emit a recovery notice")
	assert.GreaterOrEqual(t, lowNotices, 1, "the low transition must have been reported")
}

func TestRun_TierShedsOnLowBudgetAndNotices(t *testing.T) {
	// A low GraphQL budget must select a shed tier: the fetch query drops
	// annotations, and a loud degraded notice names what is no longer watched.
	// The shed snapshot must not fire false events against the full baseline.
	var lastQuery string
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			lastQuery = query
			return assign(result, QueryResponse{Repository: struct {
				PullRequest *PullRequest `json:"pullRequest"`
			}{PullRequest: mkPR("OPEN", false, "aaaaaaa", []string{"ci-build"})}})
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			rl := RateLimitResponse{}
			rl.Resources.GraphQL.Remaining = 100 // 2%: TierNoReviews
			rl.Resources.GraphQL.Limit = 5000
			rl.Resources.GraphQL.Reset = time.Now().Add(30 * time.Minute).Unix()
			return assign(result, rl)
		},
	}
	svc := &Service{API: api}

	// Seed the consumer with a full baseline first: an APPROVED review that
	// must not read as dismissed once reviews are shed.
	baseline := &PRStatus{
		State:          "OPEN",
		ReviewDecision: "APPROVED",
		ReviewAuthor:   "alice",
		FailingChecks:  []string{"ci-build"},
	}

	var got []Notification
	opts := testRunOptions()
	opts.Budget = NewBudgetGuard(svc, opts.Interval)
	opts.CursorSnapshot = baselineJSON(t, baseline)
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		polls++
		if polls >= 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}

	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	// The fetch must use the shed query (no annotations, no reviews).
	assert.NotContains(t, lastQuery, "annotations", "the low-budget tier must shed annotations")
	assert.NotContains(t, lastQuery, "reviews(", "the low-budget tier must shed reviews")

	var notice *Notification
	for i := range got {
		if got[i].Type == string(EventDegraded) && strings.Contains(got[i].Message, "no longer watching") {
			notice = &got[i]
		}
	}
	require.NotNil(t, notice, "entering a shed tier must emit a loud notice")
	assert.Contains(t, notice.Message, "annotations")
	assert.Contains(t, notice.Message, "reviews")

	// No false events: the baseline's APPROVED review must not fire a
	// dismissal, and failing checks must not read as CI-green.
	for _, n := range got {
		assert.NotEqual(t, string(EventReviewDismissed), n.Type, "a shed review must not read as dismissed")
		assert.NotEqual(t, string(EventCIAllGreen), n.Type, "a shed snapshot must not read as CI-green")
	}
}

func TestRun_TierRecoversToFullAndNotices(t *testing.T) {
	// When the budget recovers, the tier must return to full and the watcher
	// must say so loudly.
	remaining := 100
	var lastQuery string
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			lastQuery = query
			return assign(result, QueryResponse{Repository: struct {
				PullRequest *PullRequest `json:"pullRequest"`
			}{PullRequest: mkPR("OPEN", false, "aaaaaaa", []string{"ci-build"})}})
		},
		restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
			rl := RateLimitResponse{}
			rl.Resources.GraphQL.Remaining = remaining
			rl.Resources.GraphQL.Limit = 5000
			rl.Resources.GraphQL.Reset = time.Now().Add(30 * time.Minute).Unix()
			return assign(result, rl)
		},
	}
	svc := &Service{API: api}

	var got []Notification
	opts := testRunOptions()
	opts.Budget = NewBudgetGuard(svc, opts.Interval)
	ctx, cancel := context.WithCancel(context.Background())
	cur := time.Unix(0, 0).UTC()
	opts.Now = func() time.Time { return cur }
	polls := 0
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		polls++
		cur = cur.Add(31 * time.Second) // advance past the guard's refresh window
		if polls == 2 {
			remaining = 4900 // recovery becomes visible next poll
		}
		if polls >= 4 {
			cancel()
			return context.Canceled
		}
		return nil
	}

	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	var recovered *Notification
	for i := range got {
		if got[i].Type == string(EventDegraded) && strings.Contains(got[i].Message, "resuming full monitoring") {
			recovered = &got[i]
		}
	}
	require.NotNil(t, recovered, "recovering to the full tier must emit a notice")
	assert.Contains(t, lastQuery, "annotations", "the recovered tier must fetch annotations again")
}

func TestRun_QueryCostFallbackRetriesCheaperTier(t *testing.T) {
	// "Resource limits for this query exceeded" is a per-query cost error: the
	// watcher must retry once with a cheaper tier instead of failing
	// wholesale. A rate-limit 403 must NOT retry (every query costs points).
	var queries []string
	api := &fakeAPI{
		graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			queries = append(queries, query)
			if len(queries) == 1 {
				return &ghcli.GraphQLError{Errors: []ghcli.GraphQLErrorEntry{
					{Message: "Resource limits for this query exceeded. Please reduce your query"},
				}}
			}
			return assign(result, QueryResponse{Repository: struct {
				PullRequest *PullRequest `json:"pullRequest"`
			}{PullRequest: mkPR("OPEN", false, "aaaaaaa", []string{"ci-build"})}})
		},
	}
	svc := &Service{API: api}

	var got []Notification
	opts := testRunOptions()
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		polls++
		if polls >= 2 {
			cancel()
			return context.Canceled
		}
		return nil
	}

	err := Run(ctx, svc, opts, func(n Notification) { got = append(got, n) })
	require.True(t, errors.Is(err, context.Canceled))

	require.GreaterOrEqual(t, len(queries), 2, "the cost error must trigger a cheaper retry")
	assert.Contains(t, queries[0], "annotations", "first attempt is the full query")
	assert.NotContains(t, queries[1], "annotations", "the retry must shed annotations")
	for _, n := range got {
		assert.NotEqual(t, string(EventDegraded), n.Type, "a successful cheaper retry must not degrade")
	}
}

// baselineJSON serialises a PRStatus for CursorSnapshot, mirroring how the
// cursor store persists a named-instance baseline.
func baselineJSON(t *testing.T, st *PRStatus) string {
	t.Helper()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	return string(b)
}
