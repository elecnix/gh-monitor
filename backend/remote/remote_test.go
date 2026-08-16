package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// ---------------------------------------------------------------------------
// A minimal in-process backend to serve, standing in for a real external one.
// ---------------------------------------------------------------------------

type fakeBackend struct {
	updates  []backend.Update
	status   backend.Status
	watchErr error
	// seen records what the server was asked for, so the tests can assert the
	// request survived the wire.
	seen chan Request
}

func (f *fakeBackend) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	if f.seen != nil {
		f.seen <- Request{Op: OpWatch, Target: t, Options: opts}
	}
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	ch := make(chan backend.Update, len(f.updates))
	for _, u := range f.updates {
		ch <- u
	}
	close(ch)
	return ch, nil
}

func (f *fakeBackend) Read(ctx context.Context, t backend.Target) (backend.Status, error) {
	if f.seen != nil {
		f.seen <- Request{Op: OpRead, Target: t}
	}
	return f.status, nil
}

// testStatus is a Status defined by the test, standing in for a backend that
// has its own status shape.
type testStatus struct {
	State string `json:"state"`
}

func (s *testStatus) TargetKind() backend.Kind { return backend.KindPR }

func init() {
	backend.RegisterStatusDecoder(backend.KindPR, func(raw []byte) (backend.Status, error) {
		var s testStatus
		if err := jsonUnmarshal(raw, &s); err != nil {
			return nil, err
		}
		return &s, nil
	})
}

// pipeTransport connects the client to an in-process server over a pipe pair.
func pipeTransport(t *testing.T, cfg ServerConfig) Transport {
	t.Helper()
	return TransportFunc("pipe", func(ctx context.Context) (io.ReadWriteCloser, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			defer func() { _ = serverConn.Close() }()
			_ = Serve(ctx, serverConn, cfg)
		}()
		return clientConn, nil
	})
}

func prTarget() backend.Target {
	return backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 42}
}

// ---------------------------------------------------------------------------

func TestConnectReadsTheServersDeclaredSurface(t *testing.T) {
	fake := &fakeBackend{}
	tr := pipeTransport(t, ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR, backend.KindRun},
		Source: fake,
	})

	p, err := Connect(context.Background(), tr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if p.Name() != "relay" {
		t.Fatalf("Name = %q, want relay", p.Name())
	}

	// A server that declares only a Source must not be registered as a Reader,
	// or reads would be routed to a backend that cannot serve them.
	reg := backend.NewRegistry()
	reg.RegisterSource("gh", nil, backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
		return nil, errors.New("unused")
	}))
	reg.RegisterReader("gh", nil, backend.ReaderFunc(func(context.Context, backend.Target) (backend.Status, error) {
		return nil, nil
	}))
	if err := reg.Use(p); err != nil {
		t.Fatalf("Use: %v", err)
	}

	if _, name, err := reg.SourceFor(prTarget()); err != nil || name != "relay" {
		t.Fatalf("PR source = %q, %v; want relay", name, err)
	}
	if _, name, err := reg.ReaderFor(prTarget()); err != nil || name != "gh" {
		t.Fatalf("PR reader = %q, %v; want gh", name, err)
	}
	// A kind the server did not declare stays with the built-in backend.
	if _, name, err := reg.SourceFor(backend.Target{Kind: backend.KindIssue}); err != nil || name != "gh" {
		t.Fatalf("issue source = %q, %v; want gh", name, err)
	}
}

func TestWatchStreamsUpdatesAcrossTheWire(t *testing.T) {
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	fake := &fakeBackend{
		seen: make(chan Request, 1),
		updates: []backend.Update{
			{
				ID:     "u1",
				Target: prTarget(),
				Event:  backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}},
				Status: &testStatus{State: "OPEN"},
				At:     at,
			},
			{
				ID:       "u2",
				Target:   prTarget(),
				Event:    backend.Event{Type: backend.EventMerged},
				At:       at,
				Terminal: true,
			},
		},
	}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Source: fake,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ch, err := p.Watch(context.Background(), prTarget(), backend.WatchOptions{Interval: 30 * time.Second})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	var got []backend.Update
	for u := range ch {
		got = append(got, u)
	}
	if len(got) != 2 {
		t.Fatalf("got %d updates, want 2", len(got))
	}
	if got[0].Event.Type != backend.EventNewFailingChecks || got[0].ID != "u1" {
		t.Fatalf("first update = %+v", got[0])
	}
	if len(got[0].Event.Checks) != 1 || got[0].Event.Checks[0] != "build" {
		t.Fatalf("checks did not survive the wire: %+v", got[0].Event)
	}
	st, ok := got[0].Status.(*testStatus)
	if !ok || st.State != "OPEN" {
		t.Fatalf("status did not survive the wire: %#v", got[0].Status)
	}
	if !got[1].Terminal {
		t.Fatal("terminal flag did not survive the wire")
	}

	// The request reached the server intact.
	req := <-fake.seen
	if req.Op != OpWatch || req.Target.Number != 42 || req.Options.Interval != 30*time.Second {
		t.Fatalf("server saw %+v", req)
	}
}

func TestWatchClosesTheChannelWhenTheServerFinishes(t *testing.T) {
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Source: &fakeBackend{},
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ch, err := p.Watch(context.Background(), prTarget(), backend.WatchOptions{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("expected the channel to be closed with no updates")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel was not closed")
	}
}

func TestWatchReportsAServerErrorAsDegradedRatherThanSilence(t *testing.T) {
	// A backend that stops seeing the target must say so. Closing the stream
	// quietly would read as "nothing is happening", which is the one failure
	// mode a monitor must never have.
	fake := &fakeBackend{watchErr: errors.New("upstream unavailable")}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Source: fake,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ch, err := p.Watch(context.Background(), prTarget(), backend.WatchOptions{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	var got []backend.Update
	for u := range ch {
		got = append(got, u)
	}
	if len(got) != 1 {
		t.Fatalf("got %d updates, want 1 degraded update", len(got))
	}
	if got[0].Event.Type != backend.EventDegraded {
		t.Fatalf("event = %q, want degraded", got[0].Event.Type)
	}
	if got[0].Event.DegradedMessage == "" {
		t.Fatal("degraded update must carry the reason")
	}
}

func TestWatchReportsATransportDropAsDegraded(t *testing.T) {
	// Same rule for a connection that dies mid-stream: the consumer has to
	// learn the watcher went blind.
	// Connect probes the endpoint before Watch does, so the transport is
	// opened more than once; only the streaming connection drops mid-stream.
	var once sync.Once
	closed := make(chan struct{})
	tr := TransportFunc("dropping", func(ctx context.Context) (io.ReadWriteCloser, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			// Say hello and send one update, then drop the connection without
			// the end-of-stream frame.
			_ = writeJSON(serverConn, Hello{
				Protocol:     Protocol,
				Name:         "relay",
				Kinds:        []backend.Kind{backend.KindPR},
				Capabilities: []backend.Capability{backend.CapSource},
			})
			var req Request
			if err := readJSON(serverConn, &req); err != nil {
				_ = serverConn.Close() // the probe connection, closed without a request
				return
			}
			_ = writeJSON(serverConn, Frame{Update: &backend.Update{Target: prTarget(), Event: backend.Event{Type: backend.EventNewCommit}}})
			_ = serverConn.Close()
			once.Do(func() { close(closed) })
		}()
		return clientConn, nil
	})

	p, err := Connect(context.Background(), tr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ch, err := p.Watch(context.Background(), prTarget(), backend.WatchOptions{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	var got []backend.Update
	for u := range ch {
		got = append(got, u)
	}
	<-closed
	if len(got) != 2 {
		t.Fatalf("got %d updates, want the change plus a degraded notice", len(got))
	}
	if got[1].Event.Type != backend.EventDegraded {
		t.Fatalf("last event = %q, want degraded", got[1].Event.Type)
	}
}

func TestReadRoundTripsAStatus(t *testing.T) {
	fake := &fakeBackend{seen: make(chan Request, 1), status: &testStatus{State: "CLOSED"}}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Source: fake, Reader: fake,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := p.Read(context.Background(), prTarget())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, ok := st.(*testStatus)
	if !ok || got.State != "CLOSED" {
		t.Fatalf("Read returned %#v", st)
	}
	if req := <-fake.seen; req.Op != OpRead {
		t.Fatalf("server saw op %q", req.Op)
	}
}

func TestConnectRejectsAnIncompatibleProtocol(t *testing.T) {
	tr := TransportFunc("future", func(ctx context.Context) (io.ReadWriteCloser, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			_ = writeJSON(serverConn, Hello{Protocol: Protocol + 1, Name: "future"})
			_ = serverConn.Close()
		}()
		return clientConn, nil
	})
	if _, err := Connect(context.Background(), tr); err == nil {
		t.Fatal("Connect must reject a protocol it does not speak")
	}
}

func TestConnectRejectsAServerWithNoCapabilities(t *testing.T) {
	tr := TransportFunc("empty", func(ctx context.Context) (io.ReadWriteCloser, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			_ = writeJSON(serverConn, Hello{Protocol: Protocol, Name: "empty"})
			_ = serverConn.Close()
		}()
		return clientConn, nil
	})
	// Registering a backend that provides nothing would shadow nothing but
	// still show up in `gh monitor backends` as if it were useful.
	if _, err := Connect(context.Background(), tr); err == nil {
		t.Fatal("Connect must reject a server declaring no capabilities")
	}
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "unix:/tmp/x.sock", want: "unix:/tmp/x.sock"},
		{in: "tcp:127.0.0.1:9000", want: "tcp:127.0.0.1:9000"},
		{in: "exec:my-backend --flag", want: "exec:my-backend --flag"},
		{in: "", wantErr: true},
		{in: "carrier-pigeon:x", wantErr: true},
		{in: "unix:", wantErr: true},
		{in: "exec:", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseEndpoint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseEndpoint(%q) should fail", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseEndpoint(%q): %v", tc.in, err)
		}
		if got.String() != tc.want {
			t.Fatalf("ParseEndpoint(%q).String() = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

func TestUnixTransportEndToEnd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	cfg := ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &fakeBackend{updates: []backend.Update{
			{Target: prTarget(), Event: backend.Event{Type: backend.EventMerged}, Terminal: true},
		}},
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = Serve(context.Background(), conn, cfg)
			}()
		}
	}()

	tr, err := ParseEndpoint("unix:" + sock)
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	p, err := Connect(context.Background(), tr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ch, err := p.Watch(context.Background(), prTarget(), backend.WatchOptions{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	var got []backend.Update
	for u := range ch {
		got = append(got, u)
	}
	if len(got) != 1 || got[0].Event.Type != backend.EventMerged {
		t.Fatalf("got %+v", got)
	}
}
