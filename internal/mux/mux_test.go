package mux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// recordingSource is a backend.Source that records the targets it is asked
// to watch and optionally emits updates before closing.
type recordingSource struct {
	calls   []backend.Target
	updates []backend.Update
	// hold, when set, keeps the channel open until ctx is cancelled (a hub
	// stand-in for timeout tests).
	hold bool
}

func (s *recordingSource) Watch(ctx context.Context, t backend.Target, _ backend.WatchOptions) (<-chan backend.Update, error) {
	s.calls = append(s.calls, t)
	ch := make(chan backend.Update, len(s.updates)+1)
	for _, u := range s.updates {
		ch <- u
	}
	if s.hold {
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	close(ch)
	return ch, nil
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

// TestRoutingSourceRoutesByKind pins the core of issue #88: watches for a
// kind a live sub-daemon serves go to the sub-daemon; kinds it does not
// advertise go to the fallback.
func TestRoutingSourceRoutesByKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "route.sock")
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{
		updates: []backend.Update{{At: time.Now()}},
	})

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)

	fallback := &recordingSource{}
	rs := RoutingSource{Reg: reg, Fallback: fallback}

	// pr → sub-daemon: its update comes back through the returned channel.
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}, backend.WatchOptions{Once: true})
	if err != nil {
		t.Fatalf("routed watch: %v", err)
	}
	if _, ok := <-ch; !ok {
		t.Fatal("the sub-daemon's update never arrived — the watch was not routed to it")
	}
	for range ch {
	}
	if len(fallback.calls) != 0 {
		t.Fatalf("pr watch must not hit the fallback, but it was called with %v", fallback.calls)
	}

	// run → fallback (the sub-daemon does not serve it).
	if _, err := rs.Watch(ctx, backend.Target{Kind: backend.KindRun, Owner: "o", Repo: "r", RunID: 9}, backend.WatchOptions{}); err != nil {
		t.Fatalf("fallback watch: %v", err)
	}
	if len(fallback.calls) != 1 || fallback.calls[0].Kind != backend.KindRun {
		t.Fatalf("a run watch must go to the fallback; calls: %v", fallback.calls)
	}
}

// optsSource hands each watch's options to the test, emits one update, and
// holds the stream open until ctx is cancelled — a continuous sub-daemon.
type optsSource struct{ seen chan backend.WatchOptions }

func (s optsSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.seen <- opts
	ch := make(chan backend.Update, 1)
	ch <- backend.Update{Target: t, At: time.Now()}
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
	sock := shortSock(t, "resume.sock")
	sub := optsSource{seen: make(chan backend.WatchOptions, 1)}
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, sub)

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)

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
	if len(fallback.calls) != 0 {
		t.Fatalf("a resumable pr watch must not hit the fallback; calls: %v", fallback.calls)
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
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{})

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)
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
	first, ok := <-ch
	if !ok {
		t.Fatal("the fallback stream closed before saying why it served the watch")
	}
	if first.Event.Type != backend.EventDegraded || !strings.Contains(first.Event.Notice, "fakebroker") {
		t.Fatalf("first update must be a notice naming the sub-daemon, got %+v", first.Event)
	}
	if _, ok := <-ch; !ok {
		t.Fatal("the fallback's own update must follow the notice")
	}
	if len(fallback.calls) != 1 {
		t.Fatalf("the hub must serve the watch; calls: %v", fallback.calls)
	}
}

// TestRoutingSourceFailsOverWhenStreamEnds covers a sub-daemon that ends a
// continuous watch before its target is done (it restarted, or it rejected
// the target). The watch moves to the hub and says so, rather than going
// quiet while the client waits for events that will never come.
func TestRoutingSourceFailsOverWhenStreamEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "ends.sock")
	// recordingSource closes at once: the stream ends with no terminal update.
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{})

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)

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
	if len(fallback.calls) != 1 {
		t.Fatalf("the hub must take over the watch; calls: %v", fallback.calls)
	}
}

// TestRoutingSourceNoFailoverAfterTerminal: a stream that ends because the
// target reached a terminal state is finished, not broken.
func TestRoutingSourceNoFailoverAfterTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "term.sock")
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{
		updates: []backend.Update{{At: time.Now(), Terminal: true}},
	})

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)

	fallback := &recordingSource{}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 6},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(fallback.calls) != 0 {
		t.Fatalf("a terminal stream must not fail over; calls: %v", fallback.calls)
	}
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
	if len(fallback.calls) != 1 {
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
	if len(fallback.calls) != 1 {
		t.Fatal("a nil registry must route everything to the fallback")
	}
}

// compile-time guards
var (
	_ backend.Source = RoutingSource{}
	_                = errors.New
)
