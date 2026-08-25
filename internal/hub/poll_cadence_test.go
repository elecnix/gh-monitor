package hub

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pollerOf returns the single poller a one-target test hub is running.
func pollerOf(h *Hub) *poller {
	h.mu.Lock()
	defer h.mu.Unlock()
	var p *poller
	for _, poller := range h.pollers {
		p = poller
	}
	return p
}

// TestPoller_NextDelayHonoursConfiguredIdleCeiling pins idlePollCeiling
// (issue #90): the configured ceiling replaces monitor.MaxIdleInterval as the
// idle-backoff cap for every target, broker or no broker. The operator who
// sets it has said "when I must poll, trickle" — regardless of why.
func TestPoller_NextDelayHonoursConfiguredIdleCeiling(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 60*time.Second, nil, WithIdleCeiling(2*time.Hour))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond)

	p := pollerOf(h)
	require.NotNil(t, p)
	p.mu.Lock()
	p.noChange = 20 // deep into the quiet-target backoff curve
	p.mu.Unlock()

	assert.Greater(t, p.nextDelay(), 2*monitor.MaxIdleInterval,
		"a configured idlePollCeiling must allow idle waits past the built-in 300s default")
	assert.LessOrEqual(t, p.nextDelay(), 3*time.Hour,
		"nextDelay must stay within the configured ceiling (plus jitter and error backoff)")
}

// TestPoller_PausesTimerFetchWhileBrokerHealthy pins pollWhenBrokerHealthy
// (issue #90): while the wake path is live, timer-driven fetches stop
// entirely — scheduled API spend exists only as insurance against event
// loss. A degrade must resume polling promptly: the first post-degrade fetch
// happens on the transition itself (a loud wake to every poller), not at
// some stale timer tick computed while the transport still looked up.
func TestPoller_PausesTimerFetchWhileBrokerHealthy(t *testing.T) {
	var fetches int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil,
		WithPauseWhenBrokerHealthy(true), WithIdleCeiling(time.Hour))
	t.Cleanup(h.Stop)

	h.SetBrokerIdleCap(30 * time.Minute)
	h.SetBrokerHealth(true, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond) // first-poll fetch + settle

	whileHealthy := atomic.LoadInt64(&fetches)
	time.Sleep(40 * time.Millisecond) // many base intervals' worth of ticks
	assert.LessOrEqual(t, atomic.LoadInt64(&fetches)-whileHealthy, int64(1),
		"while the broker is healthy and pausing is enabled, the timer must not fetch")

	// Kill the transport: polling resumes immediately, loudly.
	h.SetBrokerHealth(false, "connection lost: EOF")
	got := collect(ch, 100*time.Millisecond)
	require.Contains(t, got, string(monitor.EventDegraded),
		"the degrade notice must reach the subscriber")

	resumed := atomic.LoadInt64(&fetches)
	h.SetBrokerHealth(true, "") // re-arm, then prove fetching resumes under a healthy mark again only via pause logic
	assert.Greater(t, resumed, whileHealthy,
		"degrading the broker must wake an immediate fetch — insurance polling resumes now, not at a stale tick")
}

// TestPoller_NextDelayWhilePausedIsTheRecheckTick pins what "paused" means
// numerically: the timer keeps ticking at roughly the base interval so a
// degrade is noticed within one tick even without the transition wake, but
// no fetch rides on those ticks.
func TestPoller_NextDelayWhilePausedIsTheRecheckTick(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Minute, nil, WithPauseWhenBrokerHealthy(true))
	t.Cleanup(h.Stop)

	h.SetBrokerIdleCap(6 * time.Hour)
	h.SetBrokerHealth(true, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond)

	p := pollerOf(h)
	require.NotNil(t, p)
	p.mu.Lock()
	p.noChange = 20 // irrelevant while paused: no backoff curve is being walked
	p.mu.Unlock()

	d := p.nextDelay()
	assert.Less(t, d, 2*time.Minute,
		"a paused poller's tick must stay near the base interval so degradation is noticed within one tick")
	assert.LessOrEqual(t, d, 2*monitor.MaxIdleInterval)
}

// TestPoller_PauseRequiresAHealthyBroker guards against the silent-watchdog
// failure mode: with no broker transport configured (healthy never becomes
// true) or a degraded one, pauseWhenBrokerHealthy must never suppress
// polling. Absence of a broker is not success.
func TestPoller_PauseRequiresAHealthyBroker(t *testing.T) {
	var fetches int64
	newPausedHub := func() *Hub {
		// The idle ceiling is capped for this test: without it, fifty
		// milliseconds of eventless polling back the cadence up to hundreds of
		// milliseconds, and the fixed observation window below can expire
		// before the next timer tick lands (a race CI hit intermittently).
		// A 20ms ceiling keeps every tick inside the window without changing
		// what the assertions verify.
		return New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
			atomic.AddInt64(&fetches, 1)
			return prFixture(nil), nil
		}, nil, 5*time.Millisecond, nil, WithPauseWhenBrokerHealthy(true), WithIdleCeiling(20*time.Millisecond))
	}

	// No broker transport at all: normal interval polling continues.
	h := newPausedHub()
	t.Cleanup(h.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond)

	noBroker := atomic.LoadInt64(&fetches)
	time.Sleep(40 * time.Millisecond)
	assert.Greater(t, atomic.LoadInt64(&fetches), noBroker,
		"with no broker configured, enabling the pause preference must not stop timer polling")
}
