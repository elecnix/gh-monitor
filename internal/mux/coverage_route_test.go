package mux

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/mux/muxtest"
)

var covTarget = backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}

// scriptHub stands in for the polling hub. A continuous watch delivers a
// first poll and stays open; a --once read delivers one catch-up snapshot.
type scriptHub struct {
	mu         sync.Mutex
	once       int
	continuous int
	opts       []backend.WatchOptions
	active     int
	failing    bool // the continuous first poll reports a failing check
}

func (h *scriptHub) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	h.mu.Lock()
	h.opts = append(h.opts, opts)
	if opts.Once {
		h.once++
	} else {
		h.continuous++
		h.active++
	}
	failing := h.failing
	h.mu.Unlock()

	ch := make(chan backend.Update, 4)
	if opts.Once {
		ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventAllClear}, Status: testStatus{Note: "catchup"}, At: time.Now()}
		close(ch)
		return ch, nil
	}
	if opts.Baseline == "" {
		ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventFirstPoll}, Status: testStatus{Note: "seed"}, At: time.Now(), More: failing}
		if failing {
			ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}}, Status: testStatus{Note: "seed"}, At: time.Now()}
		}
	}
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		h.active--
		h.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

func (h *scriptHub) counts() (once, continuous, active int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.once, h.continuous, h.active
}

// subSource stands in for a webhook sub-daemon's watch stream.
type subSource struct {
	mu       sync.Mutex
	opts     []backend.WatchOptions
	calls    int
	endFirst bool // the first watch ends right after its first poll, as a restarting sub-daemon's does
}

func (s *subSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.mu.Lock()
	s.calls++
	s.opts = append(s.opts, opts)
	end := s.endFirst && s.calls == 1
	s.mu.Unlock()
	ch := make(chan backend.Update, 2)
	ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()}
	if end {
		close(ch)
		return ch, nil
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (s *subSource) watchCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

type covRig struct {
	reg  *Registry
	cmap *CoverageMap
	feed *muxtest.CoverageFeed
	hub  *scriptHub
	sub  *subSource
	rs   RoutingSource
}

func newCovRig(t *testing.T, ctx context.Context, safety time.Duration, preload func(*CoverageMap)) *covRig {
	t.Helper()
	r := &covRig{
		cmap: NewCoverageMap(time.Hour, 150*time.Millisecond),
		feed: muxtest.NewCoverageFeed(),
		hub:  &scriptHub{},
		sub:  &subSource{},
	}
	if preload != nil {
		preload(r.cmap)
		// The sub-daemon vouches for what the map holds; a stream that did not
		// repeat a repository would, correctly, retire it.
		for _, repo := range r.cmap.Snapshot() {
			r.feed.Send(remote.CoverageEntry{Repo: repo, Covered: true, LastEvent: time.Now()})
		}
	}
	sock := shortSock(t, "cov.sock")
	muxtest.StartFakeCoverageBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, r.sub, r.feed)
	r.reg = NewRegistry(os.Stderr)
	r.reg.SetCoverage(r.cmap)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	r.reg.Track("broker", tr)
	r.reg.Probe(ctx)
	r.rs = RoutingSource{Reg: r.reg, Fallback: r.hub, SafetyInterval: safety}
	return r
}

func (r *covRig) watch(t *testing.T, ctx context.Context) (<-chan backend.Update, func() []backend.Update) {
	t.Helper()
	ch, err := r.rs.Watch(ctx, covTarget, backend.WatchOptions{ResumeID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []backend.Update
	go func() {
		for u := range ch {
			mu.Lock()
			seen = append(seen, u)
			mu.Unlock()
		}
	}()
	return ch, func() []backend.Update {
		mu.Lock()
		defer mu.Unlock()
		return append([]backend.Update(nil), seen...)
	}
}

func countType(us []backend.Update, ty backend.EventType) int {
	n := 0
	for _, u := range us {
		if u.Event.Type == ty {
			n++
		}
	}
	return n
}

func TestUncoveredRepositoryIsServedByTheHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, nil)
	rig.hub.failing = true
	_, seen := rig.watch(t, ctx)

	eventually(t, "the hub reports the failing check", func() bool {
		return countType(seen(), backend.EventNewFailingChecks) == 1
	})
	if rig.sub.watchCalls() != 0 {
		t.Fatal("an uncovered repository must not be routed to the sub-daemon")
	}
	if _, cont, _ := rig.hub.counts(); cont != 1 {
		t.Fatalf("the hub must serve the watch continuously; continuous watches = %d", cont)
	}
}

func TestCoverageReportMovesWatchWithOneFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, nil)
	_, seen := rig.watch(t, ctx)
	eventually(t, "first poll", func() bool { return countType(seen(), backend.EventFirstPoll) == 1 })

	rig.feed.Send(remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})

	eventually(t, "watch moved to the sub-daemon", func() bool { return rig.sub.watchCalls() == 1 })
	once, _, active := rig.hub.counts()
	if once != 1 {
		t.Fatalf("the move costs one catch-up fetch; got %d", once)
	}
	eventually(t, "hub watch released", func() bool { _, _, a := rig.hub.counts(); return a == 0 })
	_ = active
	rig.sub.mu.Lock()
	baseline := rig.sub.opts[0].Baseline
	rig.sub.mu.Unlock()
	if baseline != `{"note":"catchup"}` {
		t.Fatalf("the sub-daemon must start from the catch-up snapshot; Baseline = %q", baseline)
	}
	time.Sleep(100 * time.Millisecond)
	if n := countType(seen(), backend.EventFirstPoll); n != 1 {
		t.Fatalf("one watch reports one first poll; got %d", n)
	}
}

func TestWithdrawnCoverageMovesWatchBackToTheHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, func(m *CoverageMap) {
		m.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	})
	_, _ = rig.watch(t, ctx)
	eventually(t, "served by the sub-daemon", func() bool { return rig.sub.watchCalls() == 1 })

	rig.feed.Send(remote.CoverageEntry{Repo: "o/r", Covered: false})

	eventually(t, "hub resumes the watch", func() bool { _, cont, a := rig.hub.counts(); return cont == 1 && a == 1 })
	once, _, _ := rig.hub.counts()
	if once != 1 {
		t.Fatalf("only the backlog read is a one-shot fetch; got %d", once)
	}
	rig.hub.mu.Lock()
	b := rig.hub.opts[len(rig.hub.opts)-1].Baseline
	rig.hub.mu.Unlock()
	if b == "" {
		t.Fatal("the hub must resume from the watch's baseline, not replay a first poll")
	}
}

func TestDegradedPastGraceMovesWatchBackToTheHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, func(m *CoverageMap) {
		m.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	})
	_, _ = rig.watch(t, ctx)
	eventually(t, "served by the sub-daemon", func() bool { return rig.sub.watchCalls() == 1 })

	rig.feed.Send(remote.CoverageEntry{Degraded: true})
	time.Sleep(50 * time.Millisecond)
	if _, cont, _ := rig.hub.counts(); cont != 0 {
		t.Fatalf("degraded inside the grace period keeps the watch where it is; hub continuous = %d", cont)
	}
	eventually(t, "hub resumes after the grace period", func() bool { _, cont, _ := rig.hub.counts(); return cont == 1 })
}

func TestSafetyFetchRunsWhenCoveredRepositoryGoesQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 200*time.Millisecond, func(m *CoverageMap) {
		m.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	})
	_, _ = rig.watch(t, ctx)
	eventually(t, "served by the sub-daemon", func() bool { return rig.sub.watchCalls() == 1 })
	before, _, _ := rig.hub.counts()
	eventually(t, "a safety fetch", func() bool { o, _, _ := rig.hub.counts(); return o > before })
}

func TestSafetyFetchSkippedWhileEventsArrive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 300*time.Millisecond, func(m *CoverageMap) {
		m.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	})
	_, _ = rig.watch(t, ctx)
	eventually(t, "served by the sub-daemon", func() bool { return rig.sub.watchCalls() == 1 })
	before, _, _ := rig.hub.counts()
	stop := time.After(900 * time.Millisecond)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			rig.cmap.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
		case <-stop:
			break loop
		}
	}
	if after, _, _ := rig.hub.counts(); after != before {
		t.Fatalf("events kept arriving; safety fetches = %d", after-before)
	}
}

func TestPersistedCoverageRoutesToSubdaemonAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.json")
	first := NewCoverageMap(time.Hour, time.Minute)
	first.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true, LastEvent: time.Now()})
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, func(m *CoverageMap) {
		if err := m.Load(path); err != nil {
			t.Fatal(err)
		}
	})
	_, _ = rig.watch(t, ctx)
	eventually(t, "served by the sub-daemon", func() bool { return rig.sub.watchCalls() == 1 })
	if _, cont, _ := rig.hub.counts(); cont != 0 {
		t.Fatalf("a persisted entry must route without hub polling; continuous = %d", cont)
	}
}

func TestExpiredPersistedCoverageRoutesToTheHub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.json")
	old := NewCoverageMap(time.Hour, time.Minute)
	old.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	old.Apply("fakebroker", remote.CoverageEntry{Repo: "o/r", Covered: true})
	if err := old.Save(path); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig := newCovRig(t, ctx, 0, func(m *CoverageMap) { _ = m.Load(path) })
	_, _ = rig.watch(t, ctx)
	eventually(t, "served by the hub", func() bool { _, cont, _ := rig.hub.counts(); return cont == 1 })
	if rig.sub.watchCalls() != 0 {
		t.Fatal("an expired entry must not route to the sub-daemon")
	}
}

func TestSubdaemonWithoutCoverageKeepsPerKindRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := &scriptHub{}
	sub := &subSource{}
	reg := probedRegistry(t, ctx, shortSock(t, "plain.sock"), []backend.Kind{backend.KindPR}, sub)
	reg.SetCoverage(NewCoverageMap(time.Hour, time.Minute))
	rs := RoutingSource{Reg: reg, Fallback: hub, SafetyInterval: time.Minute}
	ch, err := rs.Watch(ctx, covTarget, backend.WatchOptions{ResumeID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range ch {
		}
	}()
	eventually(t, "per-kind routing to the sub-daemon", func() bool { return sub.watchCalls() == 1 })
	if _, cont, _ := hub.counts(); cont != 0 {
		t.Fatalf("a sub-daemon without the coverage capability must keep today's routing; hub continuous = %d", cont)
	}
}
