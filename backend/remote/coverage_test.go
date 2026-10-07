package remote

import (
	"context"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

type fixedCoverage struct{ entries []CoverageEntry }

func (f fixedCoverage) Coverage(ctx context.Context) (<-chan CoverageEntry, error) {
	ch := make(chan CoverageEntry, len(f.entries))
	for _, e := range f.entries {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestCoverageStreamCrossesTheWire(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := pipeTransport(t, ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR},
		Source: &fakeBackend{},
		Coverage: fixedCoverage{entries: []CoverageEntry{
			{Repo: "owner/repo", Covered: true, LastEvent: at},
			{Synced: true},
			{Repo: "owner/repo", Covered: false},
		}},
	})
	p, err := Connect(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasCoverage() {
		t.Fatal("a server with a coverage source must declare the capability")
	}
	ch, err := p.Coverage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []CoverageEntry
	for e := range ch {
		got = append(got, e)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Repo != "owner/repo" || !got[0].Covered || !got[0].LastEvent.Equal(at) {
		t.Fatalf("first entry = %+v", got[0])
	}
	if !got[1].Synced || got[2].Covered {
		t.Fatalf("entries = %+v", got)
	}
}

func TestServerWithoutCoverageDoesNotDeclareIt(t *testing.T) {
	tr := pipeTransport(t, ServerConfig{Name: "relay", Source: &fakeBackend{}})
	p, err := Connect(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if p.HasCoverage() {
		t.Fatal("a server without a coverage source must not declare the capability")
	}
	if _, err := p.Coverage(context.Background()); err == nil {
		t.Fatal("asking for coverage from a server that lacks it must fail")
	}
}
