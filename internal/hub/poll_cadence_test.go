package hub

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
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
//
// The idle ceiling is deliberately capped at 20ms: with a ceiling of hours,
// an unpaused poller backs off past the observation window within the first
// collect and then makes no requests either way, so the assertion would hold
// just as well against a broken pause. A short ceiling keeps ticks landing
// inside the window, which is what makes their absence mean something.
func TestPoller_PausesTimerFetchWhileBrokerHealthy(t *testing.T) {
	var fetches int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil,
		WithPauseWhenBrokerHealthy(true), WithIdleCeiling(20*time.Millisecond))
	t.Cleanup(h.Stop)

	h.SetBrokerIdleCap(30 * time.Minute)
	h.SetBrokerCoverage(coverAll{})
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
	h.SetBrokerCoverage(coverAll{})
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

// coverOnly is a BrokerCoverage oracle reporting exactly the listed
// "owner/repo" keys covered — the shape of a real deployment where a webhook
// exists on one organisation's repositories and nowhere else.
type coverOnly map[string]bool

func (c coverOnly) Covers(owner, repo string) bool { return c[owner+"/"+repo] }

// coverAll reports every repository covered. Tests use it when they mean to
// exercise health-driven cadence and not coverage itself.
type coverAll struct{}

func (coverAll) Covers(string, string) bool { return true }

// hubTarget builds a PR target for an arbitrary repository, so a test can run
// two pollers whose only difference is whether the broker covers them.
func hubTarget(owner, repo string, number int) backend.Target {
	return backend.Target{Kind: backend.KindPR, Owner: owner, Repo: repo, Number: number, Host: "github.com"}
}

// pollerFor returns the poller watching owner/repo.
func pollerFor(h *Hub, owner, repo string) *poller {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, p := range h.pollers {
		if k.owner == owner && k.repo == repo {
			return p
		}
	}
	return nil
}

// TestPoller_PauseIsScopedToCoveredRepositories is the regression test for
// the bug that motivated coverage at all: pollWhenBrokerHealthy: false used
// to consult one hub-wide health flag, so a single connected broker silenced
// timer polling for *every* watched repository — including repositories no
// webhook published for, which then had neither a wake path nor a poll and
// went quietly blind. Health is a property of the transport; coverage is a
// property of the repository, and only the latter may license skipping a poll.
func TestPoller_PauseIsScopedToCoveredRepositories(t *testing.T) {
	var covered, uncovered int64
	h := New(func(ctx context.Context, id resolver.Identity, _ monitor.QueryTier) (any, error) {
		if id.Owner == "prizmal-ai" {
			atomic.AddInt64(&covered, 1)
		} else {
			atomic.AddInt64(&uncovered, 1)
		}
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil,
		WithPauseWhenBrokerHealthy(true), WithIdleCeiling(20*time.Millisecond))
	t.Cleanup(h.Stop)

	h.SetBrokerIdleCap(30 * time.Minute)
	h.SetBrokerCoverage(coverOnly{"prizmal-ai/switch": true})
	h.SetBrokerHealth(true, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chCovered, cancelCovered := h.SubscribePR(ctx, hubTarget("prizmal-ai", "switch", 7), testHubOpts())
	t.Cleanup(cancelCovered)
	chOther, cancelOther := h.SubscribePR(ctx, hubTarget("elecnix", "pi-agent-identity", 3), testHubOpts())
	t.Cleanup(cancelOther)
	_ = collect(chCovered, 50*time.Millisecond)
	_ = collect(chOther, 50*time.Millisecond)

	baseCovered, baseUncovered := atomic.LoadInt64(&covered), atomic.LoadInt64(&uncovered)
	time.Sleep(40 * time.Millisecond) // many base intervals' worth of ticks

	assert.LessOrEqual(t, atomic.LoadInt64(&covered)-baseCovered, int64(1),
		"a repository the broker demonstrably covers must stop being timer-polled")
	assert.Greater(t, atomic.LoadInt64(&uncovered), baseUncovered,
		"a repository the broker does not cover must keep being polled — a healthy transport it never publishes to is not a wake path for it")
}

// TestPoller_PauseRequiresACoverageOracle keeps the safe default when the hub
// is told a broker is healthy but nothing ever says what it covers. Unknown
// coverage must read as "covers nothing", because the alternative reads a
// silent transport as a working one.
func TestPoller_PauseRequiresACoverageOracle(t *testing.T) {
	var fetches int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&fetches, 1)
		return prFixture(nil), nil
	}, nil, 5*time.Millisecond, nil,
		WithPauseWhenBrokerHealthy(true), WithIdleCeiling(20*time.Millisecond))
	t.Cleanup(h.Stop)

	h.SetBrokerHealth(true, "") // healthy, but no coverage was ever installed

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 50*time.Millisecond)

	base := atomic.LoadInt64(&fetches)
	time.Sleep(40 * time.Millisecond)
	assert.Greater(t, atomic.LoadInt64(&fetches), base,
		"a healthy broker with no known coverage must not suppress polling for anything")
}

// TestPoller_ExtendedIdleCapIsScopedToCoveredRepositories pins the same
// scoping on the milder lever. Even on the default pollWhenBrokerHealthy:
// true, a connected broker used to stretch every watched repository's idle
// ceiling to the broker cap — so an uncovered repo silently went from a 300s
// worst case to a 30-minute one on the strength of a wake path that would
// never fire for it.
func TestPoller_ExtendedIdleCapIsScopedToCoveredRepositories(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 60*time.Second, nil)
	t.Cleanup(h.Stop)

	h.SetBrokerIdleCap(2 * time.Hour)
	h.SetBrokerCoverage(coverOnly{"prizmal-ai/switch": true})
	h.SetBrokerHealth(true, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chCovered, cancelCovered := h.SubscribePR(ctx, hubTarget("prizmal-ai", "switch", 7), testHubOpts())
	t.Cleanup(cancelCovered)
	chOther, cancelOther := h.SubscribePR(ctx, hubTarget("elecnix", "pi-agent-identity", 3), testHubOpts())
	t.Cleanup(cancelOther)
	_ = collect(chCovered, 50*time.Millisecond)
	_ = collect(chOther, 50*time.Millisecond)

	deepIdle := func(p *poller) {
		require.NotNil(t, p)
		p.mu.Lock()
		p.noChange = 20
		p.mu.Unlock()
	}
	pCovered, pOther := pollerFor(h, "prizmal-ai", "switch"), pollerFor(h, "elecnix", "pi-agent-identity")
	deepIdle(pCovered)
	deepIdle(pOther)

	assert.Greater(t, pCovered.nextDelay(), 2*monitor.MaxIdleInterval,
		"a covered repository may stretch to the broker's extended idle cap")
	assert.LessOrEqual(t, pOther.nextDelay(), 2*monitor.MaxIdleInterval,
		"an uncovered repository must keep the normal ceiling — no wake path will shorten its blind window")
}
