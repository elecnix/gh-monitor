package mux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
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
// would, advertising kinds and streaming src's updates.
func startFakeBackend(t *testing.T, ctx context.Context, sockPath string, kinds []backend.Kind, src backend.Source) {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = remote.Serve(ctx, c, remote.ServerConfig{Name: "fakebroker", Kinds: kinds, Source: src})
			}(conn)
		}
	}()
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
// kind a live sub-daemon serves go to the sub-daemon; everything else —
// including kinds it does not advertise and resumable watches, whose history
// lives in the hub — goes to the fallback.
func TestRoutingSourceRoutesByKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "route.sock")
	brokerSeen := make(chan backend.Target, 4)
	startFakeBackend(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{
		updates: []backend.Update{{At: time.Now()}},
	})
	// The fake backend's source must also record: wrap via a second registry
	// probe is not enough — assert the update came back, which proves the
	// watch was served by the sub-daemon.
	_ = brokerSeen

	reg := NewRegistry(os.Stderr)
	tr, _ := remote.ParseEndpoint("unix:" + sock)
	reg.Track("broker", tr)
	reg.Probe(ctx)

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

	// A resumable pr watch → fallback too: resume state lives in the hub.
	resume := backend.WatchOptions{ResumeID: "abc"}
	if _, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}, resume); err != nil {
		t.Fatalf("resume watch: %v", err)
	}
	if len(fallback.calls) != 2 || fallback.calls[1].Kind != backend.KindPR {
		t.Fatalf("a resumable watch must go to the fallback; calls: %v", fallback.calls)
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
