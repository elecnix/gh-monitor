package mux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/mux/muxtest"
)

// shortSock returns a socket path under a short directory: macOS caps Unix
// socket paths near 104 bytes and t.TempDir() eats most of that.
func shortSock(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ghm-mux-*.d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// recordingSource is a backend.Source that records the watches it is asked
// to serve and optionally emits updates before closing.
type recordingSource struct {
	mu      sync.Mutex
	calls   []backend.Target
	opts    []backend.WatchOptions
	updates []backend.Update
	// hold, when set, keeps a continuous watch open until ctx is cancelled
	// (a hub stand-in for timeout tests). A --once watch always closes.
	hold bool
	// err, when set, fails every watch.
	err error
}

func (s *recordingSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.mu.Lock()
	s.calls = append(s.calls, t)
	s.opts = append(s.opts, opts)
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan backend.Update, len(s.updates)+1)
	for _, u := range s.updates {
		ch <- u
	}
	if s.hold && !opts.Once {
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	close(ch)
	return ch, nil
}

// watches returns a copy of the options of every watch served so far.
func (s *recordingSource) watches() []backend.WatchOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]backend.WatchOptions(nil), s.opts...)
}

// startFakeBackend serves the remote protocol on sockPath as a sub-daemon
// would, advertising kinds and streaming src's updates. It delegates to the
// shared muxtest helper.
func startFakeBackend(t *testing.T, ctx context.Context, sockPath string, kinds []backend.Kind, src backend.Source) {
	t.Helper()
	muxtest.StartFakeBackend(t, ctx, sockPath, kinds, src)
}

func TestSocketPathSanitizesName(t *testing.T) {
	dir := "/socks"
	got := SocketPath(dir, "broker-subscriber")
	want := filepath.Join(dir, "subdaemon-broker-subscriber.sock")
	if got != want {
		t.Fatalf("SocketPath = %q, want %q", got, want)
	}
	if SocketPath(dir, "a b/c") == SocketPath(dir, "broker") {
		t.Fatal("different names must not collide after sanitization")
	}
	if s := SocketPath(dir, "///"); !filepath.IsAbs(s) || filepath.Ext(s) != ".sock" {
		t.Fatalf("a name that sanitizes to nothing must still yield a usable .sock path, got %q", s)
	}
}

func TestRegistryProbeDiscoversKinds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "broker.sock")
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{})

	reg := NewRegistry(os.Stderr)
	tr, err := remote.ParseEndpoint("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	reg.Track("broker", tr)
	reg.Probe(ctx)

	if p := reg.Provider(backend.KindPR); p == nil {
		t.Fatal("a live sub-daemon serving pr must be routable after Probe")
	} else if p.Name() != "fakebroker" {
		t.Fatalf("Provider.Name = %q, want fakebroker", p.Name())
	}
	if p := reg.Provider(backend.KindRun); p != nil {
		t.Fatalf("a pr-only sub-daemon must not serve run targets (got name %q)", p.Name())
	}
}

func TestRegistryAbsentSubdaemonIsNotRoutable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "absent.sock") // nothing is listening

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("ghost", tr)
	reg.Probe(ctx) // must not panic or block on the dead path

	if p := reg.Provider(backend.KindPR); p != nil {
		t.Fatal("a dead sub-daemon must not be routable")
	}
}

func TestRegistryRunRecoversWhenSubdaemonComesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "late.sock")

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("late", tr)
	runCtx, stopRun := context.WithCancel(ctx)
	t.Cleanup(stopRun)
	go reg.Run(runCtx, 5*time.Millisecond)

	time.Sleep(15 * time.Millisecond)
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Provider(backend.KindPR) != nil {
			return // the probe loop found it
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Run's probe loop never discovered the sub-daemon that came up late")
}

// probedRegistry tracks a fake sub-daemon on sock that serves kinds from src.
func probedRegistry(t *testing.T, ctx context.Context, sock string, kinds []backend.Kind, src backend.Source) *Registry {
	t.Helper()
	startFakeBackend(t, ctx, sock, kinds, src)
	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)
	return reg
}

// backlogOnly fails the test unless the fallback served exactly one watch,
// and that watch was the one-shot backlog read.
func backlogOnly(t *testing.T, fallback *recordingSource) {
	t.Helper()
	got := fallback.watches()
	if len(got) != 1 || !got[0].Once {
		t.Fatalf("the fallback must serve only the backlog read; watches: %+v", got)
	}
}

// TestRoutingSourceRoutesByKind pins the core of issue #88: watches for a
// kind a live sub-daemon serves go to the sub-daemon; kinds it does not
// advertise go to the fallback.
func TestRoutingSourceRoutesByKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := probedRegistry(t, ctx, shortSock(t, "route.sock"), []backend.Kind{backend.KindPR}, &recordingSource{
		updates: []backend.Update{{At: time.Now()}},
		hold:    true,
	})

	fallback := &recordingSource{}
	rs := RoutingSource{Reg: reg, Fallback: fallback}

	// pr → sub-daemon: its update comes back through the returned channel.
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}, backend.WatchOptions{})
	if err != nil {
		t.Fatalf("routed watch: %v", err)
	}
	if _, ok := <-ch; !ok {
		t.Fatal("the sub-daemon's update never arrived — the watch was not routed to it")
	}
	backlogOnly(t, fallback)

	// run → fallback (the sub-daemon does not serve it).
	if _, err := rs.Watch(ctx, backend.Target{Kind: backend.KindRun, Owner: "o", Repo: "r", RunID: 9}, backend.WatchOptions{}); err != nil {
		t.Fatalf("fallback watch: %v", err)
	}
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	if len(fallback.calls) != 2 || fallback.calls[1].Kind != backend.KindRun {
		t.Fatalf("a run watch must go to the fallback; calls: %v", fallback.calls)
	}
}

// TestRoutingSourceOnceReadsTheBacklog pins issue #119 for --once: the read
// is the target's backlog, which a sub-daemon fed by events may never have
// seen, so the fallback answers it and the sub-daemon is not asked.
func TestRoutingSourceOnceReadsTheBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := &recordingSource{updates: []backend.Update{{Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()}}}
	reg := probedRegistry(t, ctx, shortSock(t, "once.sock"), []backend.Kind{backend.KindPR}, sub)

	fallback := &recordingSource{updates: []backend.Update{
		{Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now(), More: true},
		{Event: backend.Event{Type: backend.EventNewGeneralComments}, At: time.Now()},
	}}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}, backend.WatchOptions{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	var types []backend.EventType
	for u := range ch {
		types = append(types, u.Event.Type)
	}
	if len(types) != 2 || types[1] != backend.EventNewGeneralComments {
		t.Fatalf("a --once read must deliver the fallback's backlog; got %v", types)
	}
	backlogOnly(t, fallback)
	if n := len(sub.watches()); n != 0 {
		t.Fatalf("the sub-daemon must not serve a --once read; it served %d", n)
	}
}

// optsSource hands each watch's options to the test, emits its updates (one
// untyped update by default), and holds the stream open until ctx is
// cancelled — a continuous sub-daemon.
type optsSource struct {
	seen    chan backend.WatchOptions
	updates []backend.Update
}

func (s optsSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.seen <- opts
	updates := s.updates
	if updates == nil {
		updates = []backend.Update{{Target: t, At: time.Now()}}
	}
	ch := make(chan backend.Update, len(updates))
	for _, u := range updates {
		ch <- u
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestRoutingSourceRoutesResumableWatch pins issue #114: every continuous
// watch carries a ResumeID, so routing only ID-less watches meant a
// sub-daemon never served one. A watch with a ResumeID goes to the
// sub-daemon, and the ID travels with it.
func TestRoutingSourceRoutesResumableWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := optsSource{seen: make(chan backend.WatchOptions, 1)}
	reg := probedRegistry(t, ctx, shortSock(t, "resume.sock"), []backend.Kind{backend.KindPR}, sub)

	fallback := &recordingSource{}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatalf("routed watch: %v", err)
	}
	select {
	case opts := <-sub.seen:
		if opts.ResumeID != "abc" {
			t.Fatalf("sub-daemon got ResumeID %q, want abc", opts.ResumeID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a watch with a ResumeID never reached the sub-daemon")
	}
	if _, ok := <-ch; !ok {
		t.Fatal("the sub-daemon's update never arrived")
	}
	backlogOnly(t, fallback)
}

// testStatus is a Status the fallback attaches to its backlog updates.
type testStatus struct {
	Note string `json:"note"`
}

func (testStatus) TargetKind() backend.Kind { return backend.KindPR }

// TestRoutingSourceStartsWithTheBacklog pins issue #119 for a continuous
// watch: the fallback's first-poll batch comes first, the sub-daemon is
// seeded with the snapshot that batch carried, and the sub-daemon's own
// status-only first poll is dropped so the watch reports one first poll.
func TestRoutingSourceStartsWithTheBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}
	sub := optsSource{seen: make(chan backend.WatchOptions, 1), updates: []backend.Update{
		{Target: target, Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()},
		{Target: target, Event: backend.Event{Type: backend.EventReviewApproved}, At: time.Now()},
	}}
	reg := probedRegistry(t, ctx, shortSock(t, "backlog.sock"), []backend.Kind{backend.KindPR}, sub)

	fallback := &recordingSource{updates: []backend.Update{
		{Target: target, Event: backend.Event{Type: backend.EventFirstPoll}, Status: testStatus{Note: "seed"}, At: time.Now(), More: true},
		{Target: target, Event: backend.Event{Type: backend.EventNewUnresolvedThreads}, Status: testStatus{Note: "seed"}, At: time.Now()},
	}}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, target, backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	var types []backend.EventType
	for len(types) < 3 {
		select {
		case u := <-ch:
			types = append(types, u.Event.Type)
		case <-time.After(2 * time.Second):
			t.Fatalf("the watch stalled after %v", types)
		}
	}
	want := []backend.EventType{backend.EventFirstPoll, backend.EventNewUnresolvedThreads, backend.EventReviewApproved}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("got %v, want %v", types, want)
		}
	}
	if opts := <-sub.seen; opts.Baseline != `{"note":"seed"}` {
		t.Fatalf("the sub-daemon must be seeded with the backlog's snapshot; Baseline = %q", opts.Baseline)
	}
	backlogOnly(t, fallback)
}

// TestRoutingSourceReportsBacklogFailure: when the backlog read fails, the
// watch says so before the sub-daemon's own first poll, instead of starting
// with a first poll that reads as a PR with nothing on it.
func TestRoutingSourceReportsBacklogFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}
	sub := optsSource{seen: make(chan backend.WatchOptions, 1), updates: []backend.Update{
		{Target: target, Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()},
	}}
	reg := probedRegistry(t, ctx, shortSock(t, "nobacklog.sock"), []backend.Kind{backend.KindPR}, sub)

	fallback := &recordingSource{err: errors.New("no route to the hub")}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, target, backend.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first := <-ch
	if first.Event.Type != backend.EventDegraded || !strings.Contains(first.Event.Notice, "no route to the hub") {
		t.Fatalf("the first update must report the failed backlog read, got %+v", first.Event)
	}
	if second := <-ch; second.Event.Type != backend.EventFirstPoll {
		t.Fatalf("with no backlog, the sub-daemon's first poll must pass through, got %+v", second.Event)
	}
}

// TestRoutingSourceTerminalBacklogEndsWatch: a target the backlog read finds
// merged or closed is finished; there is nothing for a sub-daemon to serve.
func TestRoutingSourceTerminalBacklogEndsWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := &recordingSource{hold: true}
	reg := probedRegistry(t, ctx, shortSock(t, "merged.sock"), []backend.Kind{backend.KindPR}, sub)

	fallback := &recordingSource{updates: []backend.Update{{Event: backend.Event{Type: backend.EventMerged}, At: time.Now(), Terminal: true}}}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}, backend.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if n := len(sub.watches()); n != 0 {
		t.Fatalf("a finished target must not reach the sub-daemon; it served %d", n)
	}
}

// TestRoutingSourceReportsDialFailure pins the second half of issue #114: a
// watch whose sub-daemon dial fails falls back to the hub and says why,
// instead of silently spending the GraphQL budget the sub-daemon would have
// saved.
func TestRoutingSourceReportsDialFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "gone.sock")
	reg := probedRegistry(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{})
	// The sub-daemon dies between the probe and the watch.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}

	fallback := &recordingSource{updates: []backend.Update{{At: time.Now()}}}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 4},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatalf("a failed dial must fall back, not error: %v", err)
	}
	if _, ok := <-ch; !ok {
		t.Fatal("the backlog never arrived")
	}
	notice, ok := <-ch
	if !ok {
		t.Fatal("the fallback stream closed before saying why it served the watch")
	}
	if notice.Event.Type != backend.EventDegraded || !strings.Contains(notice.Event.Notice, "fakebroker") {
		t.Fatalf("the update after the backlog must be a notice naming the sub-daemon, got %+v", notice.Event)
	}
	if _, ok := <-ch; !ok {
		t.Fatal("the fallback's own update must follow the notice")
	}
	if got := fallback.watches(); len(got) != 2 || got[1].Once {
		t.Fatalf("the hub must serve the backlog, then the watch; watches: %+v", got)
	}
}

// TestRoutingSourceFailsOverWhenStreamEnds covers a sub-daemon that ends a
// continuous watch before its target is done (it restarted, or it rejected
// the target). The watch moves to the hub and says so, rather than going
// quiet while the client waits for events that will never come.
func TestRoutingSourceFailsOverWhenStreamEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// recordingSource closes at once: the stream ends with no terminal update.
	reg := probedRegistry(t, ctx, shortSock(t, "ends.sock"), []backend.Kind{backend.KindPR}, &recordingSource{})

	fallback := &recordingSource{hold: true}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 5},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("the watch ended with the sub-daemon's stream instead of failing over")
		}
		if u.Event.Type != backend.EventDegraded || !strings.Contains(u.Event.Notice, "fakebroker") {
			t.Fatalf("failover must be announced, got %+v", u.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failover notice")
	}
	if got := fallback.watches(); len(got) != 2 || got[1].Once {
		t.Fatalf("the hub must serve the backlog, then take over the watch; watches: %+v", got)
	}
}

// TestRoutingSourceNoFailoverAfterTerminal: a stream that ends because the
// target reached a terminal state is finished, not broken.
func TestRoutingSourceNoFailoverAfterTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := probedRegistry(t, ctx, shortSock(t, "term.sock"), []backend.Kind{backend.KindPR}, &recordingSource{
		updates: []backend.Update{{At: time.Now(), Terminal: true}},
	})

	fallback := &recordingSource{}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 6},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	backlogOnly(t, fallback)
}

func TestRoutingSourceFallsBackWhenSubdaemonDead(t *testing.T) {
	sock := shortSock(t, "dead.sock") // never served
	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("dead", tr)
	reg.Probe(context.Background())

	fallback := &recordingSource{}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	if _, err := rs.Watch(context.Background(), backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 2}, backend.WatchOptions{}); err != nil {
		t.Fatalf("a dead sub-daemon must fall back, not error: %v", err)
	}
	if len(fallback.watches()) != 1 {
		t.Fatal("the fallback must have served the watch")
	}
}

func TestRoutingSourceTimeoutStopsWatch(t *testing.T) {
	sock := shortSock(t, "none.sock")
	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("none", tr)
	reg.Probe(context.Background())

	fallback := &recordingSource{hold: true}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 3},
		backend.WatchOptions{Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("a timed-out watch must close its channel promptly")
	}
}

func TestRoutingSourceWithoutRegistryIsPureFallback(t *testing.T) {
	fallback := &recordingSource{}
	rs := RoutingSource{Reg: nil, Fallback: fallback}
	if _, err := rs.Watch(context.Background(), backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r"}, backend.WatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(fallback.watches()) != 1 {
		t.Fatal("a nil registry must route everything to the fallback")
	}
}

// compile-time guards
var (
	_ backend.Source = RoutingSource{}
	_                = errors.New
)
