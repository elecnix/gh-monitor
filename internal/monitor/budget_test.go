package monitor

import (
	"errors"
	"testing"
	"time"

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
