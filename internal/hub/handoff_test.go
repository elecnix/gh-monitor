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

// TestExportRestoreResumesWithoutReplay is the continuity contract behind the
// upgrade handoff (issue #73): a watcher that reconnects to the successor
// daemon with its ResumeID must NOT see a first-poll replay of what it was
// already shown — only what actually changed while the stream was down.
func TestExportRestoreResumesWithoutReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Predecessor daemon: PR with failing CI; the watcher has been shown it.
	h1 := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture([]string{"ci-build"}), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h1.Stop)

	opts := testHubOpts()
	opts.ResumeID = "watcher-1"
	ch1, cancel1 := h1.SubscribePR(ctx, testHubTarget(), opts)
	t.Cleanup(cancel1)
	got := collect(ch1, 200*time.Millisecond)
	require.Contains(t, got, "first-poll", "the initial watch starts with a first poll")
	require.Contains(t, got, "new-failing-checks", "the initial watch surfaces the failing CI")

	// Successor daemon: same PR, now green. The handoff carries the state.
	state := h1.ExportState()
	require.Len(t, state.Pollers, 1)
	require.Len(t, state.Resumes, 1, "the connected watcher's baseline must travel")

	// A short interval: if the subscriber has not yet drained the seeded
	// snapshot when the first fetch lands, the next tick redelivers.
	h2 := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 20*time.Millisecond, nil)
	t.Cleanup(h2.Stop)
	require.NoError(t, h2.RestoreState(state))

	ch2, cancel2 := h2.SubscribePR(ctx, testHubTarget(), opts)
	t.Cleanup(cancel2)
	resumed := collect(ch2, 300*time.Millisecond)

	assert.NotContains(t, resumed, "first-poll",
		"a resumed watch must not re-run its first poll")
	assert.NotContains(t, resumed, "new-failing-checks",
		"a resumed watch must not replay the failing CI it already reported")
	assert.Contains(t, resumed, "ci-all-green",
		"a resumed watch must deliver what changed while the stream was down")

	// A fresh watcher on the successor (no ResumeID) still gets the ordinary
	// first-poll treatment.
	fresh, cancelFresh := h2.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelFresh)
	assert.Contains(t, collect(fresh, 200*time.Millisecond), "first-poll",
		"a watcher with no history must still get a first poll")
}

// TestRestoreSeedsPollerWithoutRulesetFetch verifies that a restored poller
// uses the ruleset carried across the handoff instead of fetching it again,
// and that its seeded snapshot is served to the first subscriber immediately.
func TestRestoreSeedsPollerWithoutRulesetFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var rulesetFetches int64
	rulesetFn := func(owner, repo string) (*monitor.RulesetChecks, error) {
		atomic.AddInt64(&rulesetFetches, 1)
		return &monitor.RulesetChecks{Contexts: []string{"ci-build"}}, nil
	}
	h1 := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture([]string{"ci-build"}), nil
	}, rulesetFn, time.Hour, nil)
	t.Cleanup(h1.Stop)

	opts := testHubOpts()
	opts.ResumeID = "watcher-ruleset"
	ch1, cancel1 := h1.SubscribePR(ctx, testHubTarget(), opts)
	t.Cleanup(cancel1)
	collect(ch1, 200*time.Millisecond) // wait for the poller's first fetch

	state := h1.ExportState()
	require.NotNil(t, state.Pollers[0].Ruleset, "the cached ruleset must travel")
	require.NotNil(t, state.Pollers[0].Latest, "the last snapshot must travel")

	h2 := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture([]string{"ci-build"}), nil
	}, rulesetFn, time.Hour, nil)
	t.Cleanup(h2.Stop)
	require.NoError(t, h2.RestoreState(state))

	ch2, cancel2 := h2.SubscribePR(ctx, testHubTarget(), opts)
	t.Cleanup(cancel2)
	got := collect(ch2, 300*time.Millisecond)

	assert.NotContains(t, got, "new-failing-checks",
		"the seeded snapshot equals the watcher's baseline: nothing to report")
	assert.Equal(t, int64(1), atomic.LoadInt64(&rulesetFetches),
		"the successor must reuse the carried ruleset, not fetch it again")
}

// TestRestoreStateRejectsUnknownVersion verifies the successor refuses state
// it does not understand rather than guessing at it.
func TestRestoreStateRejectsUnknownVersion(t *testing.T) {
	h := New(nil, nil, time.Hour, nil)
	t.Cleanup(h.Stop)
	err := h.RestoreState(State{Version: StateVersion + 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

// TestResumeEntryExpires verifies a baseline held for a watcher that never
// reconnects is dropped, so a very late reconnect gets the ordinary
// first-poll treatment instead of a stale diff.
func TestResumeEntryExpires(t *testing.T) {
	origTTL := resumeTTL
	resumeTTL = -time.Second // already expired on arrival
	t.Cleanup(func() { resumeTTL = origTTL })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h1 := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture([]string{"ci-build"}), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h1.Stop)
	opts := testHubOpts()
	opts.ResumeID = "never-returns"
	ch1, cancel1 := h1.SubscribePR(ctx, testHubTarget(), opts)
	t.Cleanup(cancel1)
	collect(ch1, 200*time.Millisecond)

	h2 := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture([]string{"ci-build"}), nil
	}, nil, 20*time.Millisecond, nil)
	t.Cleanup(h2.Stop)
	require.NoError(t, h2.RestoreState(h1.ExportState()))

	ch2, cancel2 := h2.SubscribePR(ctx, testHubTarget(), opts)
	t.Cleanup(cancel2)
	got := collect(ch2, 300*time.Millisecond)
	assert.Contains(t, got, "first-poll",
		"an expired resume must fall back to the ordinary first poll")
}

// TestExportWhileRunning verifies ExportState is safe to call while pollers
// and subscribers are live — the race the real handoff runs, since the
// predecessor exports while every watcher is still connected.
func TestExportWhileRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, 10*time.Millisecond, nil)
	t.Cleanup(h.Stop)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				h.ExportState()
			}
		}
	}()

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	assert.NotEmpty(t, collect(ch, 200*time.Millisecond))
}
