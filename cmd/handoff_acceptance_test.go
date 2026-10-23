package cmd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// TestHandoff_RepeatedHandoffsKeepTheFleet is the acceptance test for the
// property the operator asked for: clients are never restarted, and no
// client and no baseline is lost, however many handoffs happen.
//
// It runs three successive takeovers over one socket with three connected
// watchers, and after each asserts the invariants that make a handoff
// seamless rather than merely quick: the socket path never goes away, every
// watcher is still connected under its own resume id, its baseline travelled,
// and — the one a restart would break — no watcher has been shown a second
// first poll.
func TestHandoff_RepeatedHandoffsKeepTheFleet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock, _ := startTestDaemon(t, ctx, openPR)

	const clients = 3
	resumes := make([]string, clients)
	var streams []<-chan backend.Update
	for i := 0; i < clients; i++ {
		resumes[i] = "watcher-" + string(rune('a'+i))
		streams = append(streams, connectWatcher(t, ctx, sock, resumes[i]))
	}

	// Watch every stream for a second first poll while the handoffs run.
	var mu sync.Mutex
	replays := make(map[int]int)
	var wg sync.WaitGroup
	for i, ch := range streams {
		wg.Add(1)
		go func(i int, ch <-chan backend.Update) {
			defer wg.Done()
			for {
				select {
				case u, open := <-ch:
					if !open {
						return
					}
					if u.Event.Type == backend.EventFirstPoll {
						mu.Lock()
						replays[i]++
						mu.Unlock()
					}
				case <-ctx.Done():
					return
				}
			}
		}(i, ch)
	}
	// Cancel before waiting: the readers exit on ctx cancellation, and a
	// cleanup registered earlier (t.Cleanup(cancel)) would run after this one.
	t.Cleanup(func() { cancel(); wg.Wait() })

	for round := 1; round <= 3; round++ {
		l2, state, err := listenOrAdopt(ctx, sock)
		require.NoError(t, err, "round %d: the successor must adopt the socket", round)
		require.NotNil(t, state, "round %d: state must travel", round)
		assert.Len(t, state.Resumes, clients, "round %d: every connected watcher's baseline must travel", round)
		assert.Len(t, state.Pollers, 1, "round %d: one poller per watched identity travels, not one per client", round)

		// The successor serves the same path with the carried state.
		h2 := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
			return openPR(), nil
		}, nil, time.Hour, nil)
		t.Cleanup(h2.Stop)
		require.NoError(t, h2.RestoreState(*state))
		srv := bindTestServerOn(t, ctx, h2, l2)

		// Every watcher must re-attach under its own resume id.
		require.Eventually(t, func() bool {
			info, err := handoff.Info(ctx, sock)
			if err != nil {
				return false
			}
			seen := map[string]bool{}
			for _, c := range info.Clients {
				for _, r := range c.Resumes {
					seen[r] = true
				}
			}
			for _, r := range resumes {
				if !seen[r] {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "round %d: every watcher must re-attach with its resume id", round)
		_ = srv

		// Give the streams a moment to settle, then check nobody was replayed.
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		for i := 0; i < clients; i++ {
			assert.Zero(t, replays[i], "round %d: watcher %d must not see a second first poll", round, i)
		}
		mu.Unlock()

		// The socket path must exist throughout: no client can find it missing.
		require.FileExists(t, sock, "round %d: the socket path must never disappear", round)
	}
}

// TestHandoff_NoClientEverPollsAlone pins the rule that ended the accumulation:
// a client has no polling path of its own. Its only source is the daemon, so a
// daemon that cannot be reached is an error, never a silent fallback.
func TestHandoff_NoClientEverPollsAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	t.Setenv("GH_MONITOR_AUTOSTART", "0")
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-nowhere-*.d"))

	reg := backend.NewRegistry()
	unusedBuiltin(reg)
	_, err := attachDaemon(ctx, reg, daemonTarget(), time.Minute)
	require.Error(t, err, "without a daemon a client must fail, not poll in process")
	assert.Contains(t, err.Error(), "no shared poller is listening",
		"the error must name the daemon as the missing piece")
}
