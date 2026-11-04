package cmd

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/broker"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/mux"
	"github.com/elecnix/gh-monitor/internal/mux/muxtest"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelayAndSubdaemonServeDifferentOwnersAtOnce is the coexistence proof for
// two webhook sources on one daemon. A sub-daemon child covers owner "child";
// the daemon's own wake transport, pointed at a webhook relay, covers owner
// "relay". Neither source knows about the other, and each owner's watches
// follow the source that covers them:
//
//   - the child's repository is served by the child, and the hub fetches it
//     only for the backlog it hands over;
//   - the relay's repository is served by the hub, whose timer is paused while
//     the relay is healthy, so it costs one fetch (the backlog) and no more;
//   - when the relay drops, its watch gets a visible degraded notice and polling
//     resumes at once, while the child's watch is untouched.
func TestRelayAndSubdaemonServeDifferentOwnersAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	fetches := map[string]int{}
	count := func(owner string) int {
		mu.Lock()
		defer mu.Unlock()
		return fetches[owner]
	}
	h := hub.New(func(_ context.Context, id resolver.Identity, _ monitor.QueryTier) (any, error) {
		mu.Lock()
		fetches[id.Owner]++
		mu.Unlock()
		return &monitor.PullRequest{State: "OPEN", Mergeable: "MERGEABLE"}, nil
	}, nil, 20*time.Millisecond, nil, hub.WithPauseWhenBrokerHealthy(true), hub.WithIdleCeiling(20*time.Millisecond))
	t.Cleanup(h.Stop)

	// The relay: an event for relay/repo is delivered, then the transport is healthy.
	cov := broker.NewCoverage(time.Hour)
	h.SetBrokerCoverage(cov)
	require.True(t, cov.Note("relay", "repo"))
	h.SetBrokerHealth(true, "")

	// The child: a sub-daemon that vouches for child/repo.
	sub := &holdingSubdaemonSource{resumeIDs: make(chan string, 4)}
	feed := muxtest.NewCoverageFeed()
	feed.Send(remote.CoverageEntry{Repo: "child/repo", Covered: true, LastEvent: time.Now()})
	sock := shortSocket(t, "ghmon-relay-cov-*.d")
	muxtest.StartFakeCoverageBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, sub, feed)
	cmap := mux.NewCoverageMap(time.Hour, time.Minute)
	reg := mux.NewRegistry(io.Discard)
	reg.SetCoverage(cmap)
	tr, err := remote.ParseEndpoint("unix:" + sock)
	require.NoError(t, err)
	reg.Track("child", tr)
	reg.Probe(ctx)
	rs := mux.RoutingSource{Reg: reg, Fallback: hubSource{hub: h}, HubInterval: 20 * time.Millisecond}

	relayWatch := backend.Target{Kind: backend.KindPR, Owner: "relay", Repo: "repo", Number: 1}
	childWatch := backend.Target{Kind: backend.KindPR, Owner: "child", Repo: "repo", Number: 2}

	var seenMu sync.Mutex
	var relayEvents []backend.Update
	rch, err := rs.Watch(ctx, relayWatch, backend.WatchOptions{ResumeID: "r1"})
	require.NoError(t, err)
	go func() {
		for u := range rch {
			seenMu.Lock()
			relayEvents = append(relayEvents, u)
			seenMu.Unlock()
		}
	}()
	var childEvents atomic.Int64
	cch, err := rs.Watch(ctx, childWatch, backend.WatchOptions{ResumeID: "c1"})
	require.NoError(t, err)
	go func() {
		for u := range cch {
			if u.Event.Type == backend.EventDegraded {
				childEvents.Add(1)
			}
		}
	}()

	require.Eventually(t, func() bool { return count("relay") >= 1 && sub.watches.Load() >= 1 }, 3*time.Second, 10*time.Millisecond,
		"the relay's PR is fetched by the hub and the child's PR is served by the child")

	// While the relay is healthy and covers its repository, the paused hub
	// timer adds no polls for it. The child's repository costs the hub only
	// the backlog fetch it makes before handing the watch over.
	childBacklog := count("child")
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, 1, count("relay"), "a covered repository costs its backlog fetch and nothing more while the relay is up")
	assert.Equal(t, childBacklog, count("child"), "the hub must not poll what the child covers")
	assert.Equal(t, int64(1), sub.watches.Load(), "the child serves its owner")

	// The relay goes away: a loud notice, and polling resumes.
	h.SetBrokerHealth(false, "relay unreachable")
	require.Eventually(t, func() bool {
		seenMu.Lock()
		defer seenMu.Unlock()
		for _, u := range relayEvents {
			if u.Event.Type == backend.EventDegraded && strings.Contains(u.Event.Notice, "relay unreachable") {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "the relay's watch must be told polling took over")
	require.Eventually(t, func() bool { return count("relay") >= 3 }, 3*time.Second, 10*time.Millisecond,
		"polling resumes for the relay's repository")
	assert.Equal(t, childBacklog, count("child"), "the relay's outage does not move the child's repository onto the hub")
	assert.Zero(t, childEvents.Load(), "the relay's outage is not announced on the child's watch")
}
