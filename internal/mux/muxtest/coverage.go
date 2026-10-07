package muxtest

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
)

// CoverageFeed is a controllable coverage source for a fake sub-daemon: each
// stream gets the current set first, then every entry sent afterwards.
type CoverageFeed struct {
	mu   sync.Mutex
	set  map[string]remote.CoverageEntry
	subs map[chan remote.CoverageEntry]struct{}
}

// NewCoverageFeed returns an empty feed.
func NewCoverageFeed() *CoverageFeed {
	return &CoverageFeed{set: map[string]remote.CoverageEntry{}, subs: map[chan remote.CoverageEntry]struct{}{}}
}

// Send publishes one entry to every open stream and remembers it.
func (f *CoverageFeed) Send(e remote.CoverageEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.Repo != "" {
		if e.Covered {
			f.set[e.Repo] = e
		} else {
			delete(f.set, e.Repo)
		}
	}
	for ch := range f.subs {
		ch <- e
	}
}

// Coverage implements remote.CoverageSource.
func (f *CoverageFeed) Coverage(ctx context.Context) (<-chan remote.CoverageEntry, error) {
	ch := make(chan remote.CoverageEntry, 64)
	f.mu.Lock()
	for _, e := range f.set {
		ch <- e
	}
	ch <- remote.CoverageEntry{Synced: true}
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	out := make(chan remote.CoverageEntry)
	go func() {
		defer close(out)
		defer func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
		}()
		for {
			select {
			case e := <-ch:
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// StartFakeCoverageBackend is StartFakeBackend for a sub-daemon that also
// declares the coverage capability.
func StartFakeCoverageBackend(t *testing.T, ctx context.Context, sockPath string, kinds []backend.Kind, src backend.Source, feed *CoverageFeed) {
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
				_ = remote.Serve(ctx, c, remote.ServerConfig{Name: "fakebroker", Kinds: kinds, Source: src, Coverage: feed})
			}(conn)
		}
	}()
}
