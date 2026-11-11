package hub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// TestHub_BroadcastNotice_ReachesEverySubscriberOnce: the outage and
// recovered notices (issue #223) ride the same path as the broker health
// notice, so one call is one notice per subscriber, with the text as given.
func TestHub_BroadcastNotice_ReachesEverySubscriberOnce(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	_ = collect(ch, 100*time.Millisecond) // drain the first-poll

	h.BroadcastNotice("⚠️ broker transport down for 2m0s")

	var got []backend.Update
	deadline := time.After(asyncDeadline)
	for len(got) == 0 {
		select {
		case u := <-ch:
			got = append(got, u)
		case <-deadline:
			t.Fatal("the notice never arrived")
		}
	}
	require.Len(t, got, 1)
	assert.Equal(t, monitor.EventDegraded, got[0].Event.Type)
	assert.Equal(t, "⚠️ broker transport down for 2m0s", got[0].Event.Notice)
	assert.Empty(t, collect(ch, 50*time.Millisecond), "one call is one notice")
}
