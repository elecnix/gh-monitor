package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitDegradedDeadline is waitDegraded with a caller-chosen deadline, for
// tests whose failing fetch itself takes longer than the default 2 s.
func waitDegradedDeadline(t *testing.T, ch <-chan backend.Update, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatalf("subscription closed: %s", msg)
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.DegradedMessage != "" {
				return
			}
		case <-deadline:
			t.Fatalf("timed out: %s", msg)
		}
	}
}

// TestPoller_BlindWindowStartsAtLastSuccess pins the honest semantics of
// DegradedFrom (issue #99, review finding on #102): the blind window opens at
// the last successful observation, not when the first failed fetch happens.
// Those two moments differ by up to a full poll interval — with this bug the
// declared window starts too late, so events between the last success and the
// failure fall outside it and the recovery notice claims they were observed
// when they were not. A window narrower than the truth reads as authoritative,
// which is the exact failure class #102 exists to fix.
//
// The test drives a success, then a failure, then a recovery, and requires
// DegradedFrom to land within the success window and before the failure —
// with the bug it equals the failure time and fails the assertion.
func TestPoller_BlindWindowStartsAtLastSuccess(t *testing.T) {
	var successAt, failAt time.Time
	calls := 0
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		calls++
		switch calls {
		case 1:
			time.Sleep(20 * time.Millisecond) // widen the success/failure gap so the timestamps cannot collide
			successAt = time.Now()
			return prFixture(nil), nil
		case 2:
			// Stay in the failing fetch for over two seconds past the success.
			// DegradedFrom is RFC 3339 (second precision), so the buggy value
			// (failure time truncated) can sit just under a second after the
			// success and before the raw failure stamp — a one-second gap would
			// not separate the two behaviours. Two seconds guarantees the buggy
			// value fails the assertions below.
			time.Sleep(2100 * time.Millisecond)
			failAt = time.Now()
			return nil, errors.New("gh api failed: exit status 1")
		default:
			return prFixture(nil), nil
		}
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	// Wait for the first (successful) fetch to complete before forcing the
	// failing one, so the two observations are ordered and timestamped.
	require.Eventually(t, func() bool { return !successAt.IsZero() },
		2*time.Second, 5*time.Millisecond, "the first fetch must succeed")

	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	waitDegradedDeadline(t, ch, 5*time.Second, "the second fetch must fail and broadcast")

	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	var from time.Time
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed")
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.Notice != "" && strings.Contains(u.Event.Notice, "recovered") {
				require.NotEmpty(t, u.Event.DegradedFrom, "the recovery notice must declare the blind window")
				var err error
				from, err = time.Parse(time.RFC3339, u.Event.DegradedFrom)
				require.NoError(t, err, "DegradedFrom must be RFC 3339")
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for the recovery notice")
		}
	}
done:
	require.False(t, successAt.IsZero() || failAt.IsZero(), "test bookkeeping: both fetches must have run")
	// Fixed: DegradedFrom is the success timestamp truncated to the second —
	// within one second of successAt, and over a second before the failure.
	// Buggy (DegradedFrom = failure time): it sits over a second after the
	// success and at/after the failure stamp, failing both assertions.
	assert.WithinDuration(t, successAt, from, time.Second,
		"DegradedFrom (%s) must be the last successful observation (%s), not the failure time", from, successAt)
	assert.Greater(t, failAt.Sub(from), time.Second,
		"DegradedFrom (%s) must precede the failure (%s) by more than a second: the window opens at the last success, not its discovery",
		from, failAt)
}

// TestPoller_FirstFetchFailureDeclaresNoWindow verifies the no-prior-success
// case: when a poller has never observed anything, the blind window's start
// is honestly unknowable, so the recovery must declare no interval at all —
// a zero lastSuccess suppresses the declaration rather than stamping the
// failure time as a precise-looking start.
func TestPoller_FirstFetchFailureDeclaresNoWindow(t *testing.T) {
	calls := 0
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("gh api failed: exit status 1")
		}
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	waitDegraded(t, ch, "the first fetch must fail and broadcast")

	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed")
			}
			if u.Event.Type == monitor.EventDegraded && u.Event.Notice != "" && strings.Contains(u.Event.Notice, "recovered") {
				assert.Empty(t, u.Event.DegradedFrom,
					"with no prior successful observation the window start is unknowable: the recovery must not stamp a precise-looking interval")
				assert.Empty(t, u.Event.DegradedTo,
					"no window declared means no window end either")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the recovery notice")
		}
	}
}
