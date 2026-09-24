package monitor

import (
	"errors"
	"net/http"
	"strconv"
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

// graphqlHeaders builds the rate-limit headers GitHub sends on a GraphQL
// response.
func graphqlHeaders(remaining, limit int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Resource", "graphql")
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// TestBudgetGuard_HeadersOverrideRateLimitEndpoint reproduces issue #123:
// GET /rate_limit reported used 0 while the X-RateLimit-* headers of a real
// GraphQL call in the same minute read 5000 of 5000. The guard must decide
// from the headers.
func TestBudgetGuard_HeadersOverrideRateLimitEndpoint(t *testing.T) {
	now := time.Now()
	store := ghcli.NewRateLimitStore()
	store.Observe("github.com", graphqlHeaders(0, 5000, now.Add(20*time.Minute)), now)

	g := NewBudgetGuard(&Service{API: rateLimitAPI(5000, 5000)}, 60*time.Second)
	g.UseObserved(store, "github.com")

	remaining, limit, ok := g.GraphQLRemaining(now)
	require.True(t, ok)
	assert.Equal(t, 0, remaining, "the header reading wins over /rate_limit")
	assert.Equal(t, 5000, limit)
	assert.True(t, g.Stretch(now).Low)

	resetAt, exhausted := g.GraphQLExhausted(now)
	assert.True(t, exhausted)
	assert.Equal(t, now.Add(20*time.Minute).Unix(), resetAt.Unix())
}

// TestBudgetGuard_StaleHeadersFallBackToRateLimitEndpoint: once the reset in
// the last reading has passed, that reading describes a spent window, so the
// guard asks /rate_limit until a new response arrives.
func TestBudgetGuard_StaleHeadersFallBackToRateLimitEndpoint(t *testing.T) {
	now := time.Now()
	store := ghcli.NewRateLimitStore()
	store.Observe("github.com", graphqlHeaders(0, 5000, now.Add(-time.Minute)), now.Add(-10*time.Minute))

	g := NewBudgetGuard(&Service{API: rateLimitAPI(4800, 5000)}, 60*time.Second)
	g.UseObserved(store, "github.com")

	remaining, _, ok := g.GraphQLRemaining(now)
	require.True(t, ok)
	assert.Equal(t, 4800, remaining)
	_, exhausted := g.GraphQLExhausted(now)
	assert.False(t, exhausted)
}

// TestBudgetGuard_NoRateLimitCallWhileHeadersAreFresh: /rate_limit is the
// last resort, so a fresh header reading answers without calling it.
func TestBudgetGuard_NoRateLimitCallWhileHeadersAreFresh(t *testing.T) {
	now := time.Now()
	store := ghcli.NewRateLimitStore()
	store.Observe("github.com", graphqlHeaders(3000, 5000, now.Add(20*time.Minute)), now)

	api := &fakeAPI{restFunc: func(string, string, map[string]string, interface{}, interface{}) error {
		t.Fatal("GET /rate_limit called although a fresh header reading exists")
		return nil
	}}
	g := NewBudgetGuard(&Service{API: api}, 60*time.Second)
	g.UseObserved(store, "github.com")

	remaining, _, ok := g.GraphQLRemaining(now)
	require.True(t, ok)
	assert.Equal(t, 3000, remaining)
}
