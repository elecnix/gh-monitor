package mux

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/mux/muxtest"
)

// TestWatchReturnsToSubdaemonAfterItResyncs: a sub-daemon that ends a watch
// early hands it to the hub. Once the sub-daemon sends a new full set, as it
// does after a restart, the watch goes back to it instead of being polled for
// the rest of its life.
func TestWatchReturnsToSubdaemonAfterItResyncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, func(m *CoverageMap) {
		m.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	})
	rig.sub.endFirst = true
	eventually(t, "the first full set", func() bool { return rig.cmap.Epoch("fakebroker") == 1 })
	_, _ = rig.watch(t, ctx)
	eventually(t, "the hub takes over the ended watch", func() bool { _, cont, _ := rig.hub.counts(); return cont == 1 })

	time.Sleep(100 * time.Millisecond)
	if n := rig.sub.watchCalls(); n != 1 {
		t.Fatalf("without a re-sync the watch stays on the hub; sub-daemon watches = %d", n)
	}

	rig.cmap.Retain("fakebroker", map[string]bool{"o/r": true})

	eventually(t, "the watch returns to the sub-daemon", func() bool { return rig.sub.watchCalls() == 2 })
	eventually(t, "the hub stops polling it", func() bool { _, _, a := rig.hub.counts(); return a == 0 })
}

// TestWatchStartedBeforeSubdaemonIsLiveMovesToIt: a watch that arrives before
// the first probe (a daemon start, or a sub-daemon restart) is polled by the
// hub, then moves to the sub-daemon once it is live and covers the repository.
func TestWatchStartedBeforeSubdaemonIsLiveMovesToIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmap := NewCoverageMap(time.Hour, time.Minute)
	cmap.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	feed := muxtest.NewCoverageFeed()
	feed.Send(remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	hub, sub := &scriptHub{}, &subSource{}
	sock := shortSock(t, "early.sock")
	muxtest.StartFakeCoverageBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, sub, feed)
	reg := NewRegistry(os.Stderr)
	reg.SetCoverage(cmap)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	rs := RoutingSource{Reg: reg, Fallback: hub}

	ch, err := rs.Watch(ctx, covTarget, backend.WatchOptions{ResumeID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range ch {
		}
	}()
	eventually(t, "the hub serves it while no sub-daemon is live", func() bool { _, cont, _ := hub.counts(); return cont == 1 })

	reg.Probe(ctx)

	eventually(t, "the watch moves to the sub-daemon", func() bool { return sub.watchCalls() == 1 })
	eventually(t, "the hub stops polling it", func() bool { _, _, a := hub.counts(); return a == 0 })
}

// gatedHub delivers a continuous watch's first poll as a two-update batch and
// holds the second update until release is closed.
type gatedHub struct {
	scriptHub
	release chan struct{}
}

func (h *gatedHub) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	if opts.Once || opts.Baseline != "" {
		return h.scriptHub.Watch(ctx, t, opts)
	}
	h.mu.Lock()
	h.continuous++
	h.mu.Unlock()
	ch := make(chan backend.Update, 4)
	ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventFirstPoll}, Status: testStatus{Note: "seed"}, At: time.Now(), More: true}
	go func() {
		defer close(ch)
		select {
		case <-h.release:
		case <-ctx.Done():
			return
		}
		if ctx.Err() != nil {
			return
		}
		ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}}, Status: testStatus{Note: "seed"}, At: time.Now()}
		<-ctx.Done()
	}()
	return ch, nil
}

// TestCoverageChangeMidBatchKeepsTheRestOfTheBatch: a move waits for the
// batch in flight to finish. Moving in the middle would carry the batch's
// status as the baseline, so the catch-up fetch would find nothing new and
// the rest of the batch would never be reported.
func TestCoverageChangeMidBatchKeepsTheRestOfTheBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, nil)
	gh := &gatedHub{release: make(chan struct{})}
	rig.rs.Fallback = gh
	_, seen := rig.watch(t, ctx)
	eventually(t, "first poll", func() bool { return countType(seen(), backend.EventFirstPoll) == 1 })

	rig.feed.Send(remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	eventually(t, "coverage recorded", func() bool { _, _, ok := rig.cmap.Covered("o", "r"); return ok })
	time.Sleep(100 * time.Millisecond)
	close(gh.release)

	eventually(t, "the rest of the batch is delivered", func() bool {
		return countType(seen(), backend.EventNewFailingChecks) == 1
	})
	eventually(t, "the watch then moves", func() bool { return rig.sub.watchCalls() == 1 })
	if n := countType(seen(), backend.EventFirstPoll); n != 1 {
		t.Fatalf("one watch reports one first poll; got %d", n)
	}
}
