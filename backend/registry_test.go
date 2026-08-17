package backend

import (
	"context"
	"errors"
	"testing"
)

// stubSource records nothing; identity is compared by pointer.
type stubSource struct{ id string }

func (s *stubSource) Watch(context.Context, Target, WatchOptions) (<-chan Update, error) {
	ch := make(chan Update)
	close(ch)
	return ch, nil
}

type stubReader struct{ id string }

func (r *stubReader) Read(context.Context, Target) (Status, error) { return nil, nil }

func TestRegistrySourceForFallsBackToCatchAll(t *testing.T) {
	reg := NewRegistry()
	catchAll := &stubSource{id: "poll"}
	reg.RegisterSource("poll", nil, catchAll)

	got, name, err := reg.SourceFor(Target{Kind: KindIssue})
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if got != catchAll {
		t.Fatal("expected the catch-all source")
	}
	if name != "poll" {
		t.Fatalf("name = %q, want poll", name)
	}
}

func TestRegistrySourceForPrefersKindSpecific(t *testing.T) {
	reg := NewRegistry()
	catchAll := &stubSource{id: "poll"}
	specific := &stubSource{id: "relay"}
	reg.RegisterSource("poll", nil, catchAll)
	reg.RegisterSource("relay", []Kind{KindPR, KindRun}, specific)

	// A kind the specific backend covers resolves to it, even though the
	// catch-all also matches: partial registration must win for its kinds.
	got, name, err := reg.SourceFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if got != specific || name != "relay" {
		t.Fatalf("PR should resolve to relay, got %q", name)
	}

	// A kind it does not cover falls back to the catch-all, so registering a
	// partial surface never blinds the kinds it left alone.
	got, name, err = reg.SourceFor(Target{Kind: KindIssue})
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if got != catchAll || name != "poll" {
		t.Fatalf("issue should resolve to poll, got %q", name)
	}
}

func TestRegistryLastRegistrationWinsAmongEquals(t *testing.T) {
	reg := NewRegistry()
	first := &stubSource{id: "first"}
	second := &stubSource{id: "second"}
	reg.RegisterSource("first", []Kind{KindPR}, first)
	reg.RegisterSource("second", []Kind{KindPR}, second)

	got, name, err := reg.SourceFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if got != second || name != "second" {
		t.Fatalf("expected the later registration to win, got %q", name)
	}
}

func TestRegistryNoSourceIsALoudError(t *testing.T) {
	reg := NewRegistry()
	_, _, err := reg.SourceFor(Target{Kind: KindPR})
	if !errors.Is(err, ErrNoBackend) {
		t.Fatalf("want ErrNoBackend, got %v", err)
	}
}

func TestRegistryPinSelectsByName(t *testing.T) {
	reg := NewRegistry()
	catchAll := &stubSource{id: "poll"}
	specific := &stubSource{id: "relay"}
	reg.RegisterSource("poll", nil, catchAll)
	reg.RegisterSource("relay", []Kind{KindPR}, specific)

	if err := reg.Pin("poll"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got, name, err := reg.SourceFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if got != catchAll || name != "poll" {
		t.Fatalf("pinned backend should win, got %q", name)
	}
}

func TestRegistryPinUnknownNameFails(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterSource("poll", nil, &stubSource{})
	if err := reg.Pin("nope"); err == nil {
		t.Fatal("pinning an unregistered name must fail loudly")
	}
}

func TestRegistryPinnedBackendMissingKindIsAnError(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterSource("poll", nil, &stubSource{})
	reg.RegisterSource("relay", []Kind{KindPR}, &stubSource{})
	if err := reg.Pin("relay"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	// Pinning is explicit: silently falling back to another backend would
	// contradict what the operator asked for, so this must fail.
	if _, _, err := reg.SourceFor(Target{Kind: KindIssue}); err == nil {
		t.Fatal("a pinned backend that does not cover the kind must error")
	}
}

func TestRegistryReaderResolution(t *testing.T) {
	reg := NewRegistry()
	rd := &stubReader{id: "gh"}
	reg.RegisterReader("gh", nil, rd)

	got, name, err := reg.ReaderFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("ReaderFor: %v", err)
	}
	if got != rd || name != "gh" {
		t.Fatalf("ReaderFor = %q", name)
	}
}

func TestRegistrySourceAndReaderAreIndependent(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterReader("gh", nil, &stubReader{})
	reg.RegisterSource("relay", []Kind{KindPR}, &stubSource{})

	// A backend that registers only a Source leaves reads to the backend that
	// registered a Reader — that is the whole point of separate capabilities.
	if _, name, err := reg.SourceFor(Target{Kind: KindPR}); err != nil || name != "relay" {
		t.Fatalf("SourceFor = %q, %v", name, err)
	}
	if _, name, err := reg.ReaderFor(Target{Kind: KindPR}); err != nil || name != "gh" {
		t.Fatalf("ReaderFor = %q, %v", name, err)
	}
}

// countingProvider registers a partial surface, mirroring a real external
// backend that only implements notifications for a subset of kinds.
type countingProvider struct {
	name  string
	calls int
}

func (p *countingProvider) Name() string { return p.name }

func (p *countingProvider) Register(r *Registry) error {
	p.calls++
	r.RegisterSource(p.name, []Kind{KindPR}, &stubSource{})
	return nil
}

func TestRegistryUseProvider(t *testing.T) {
	reg := NewRegistry()
	p := &countingProvider{name: "relay"}
	if err := reg.Use(p); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("Register called %d times, want 1", p.calls)
	}
	if _, name, err := reg.SourceFor(Target{Kind: KindPR}); err != nil || name != "relay" {
		t.Fatalf("SourceFor = %q, %v", name, err)
	}
}

func TestRegistryUsePropagatesProviderError(t *testing.T) {
	reg := NewRegistry()
	want := errors.New("boom")
	err := reg.Use(providerFunc{name: "bad", fn: func(*Registry) error { return want }})
	if !errors.Is(err, want) {
		t.Fatalf("Use should propagate the provider error, got %v", err)
	}
}

type providerFunc struct {
	name string
	fn   func(*Registry) error
}

func (p providerFunc) Name() string               { return p.name }
func (p providerFunc) Register(r *Registry) error { return p.fn(r) }

func TestRegistryListReportsCapabilitiesPerBackend(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterSource("gh", nil, &stubSource{})
	reg.RegisterReader("gh", nil, &stubReader{})
	reg.RegisterSource("relay", []Kind{KindRun, KindPR}, &stubSource{})

	infos := reg.List()
	if len(infos) != 2 {
		t.Fatalf("List() returned %d backends, want 2", len(infos))
	}

	byName := map[string]Info{}
	for _, i := range infos {
		byName[i.Name] = i
	}

	gh := byName["gh"]
	if len(gh.Capabilities) != 2 {
		t.Fatalf("gh capabilities = %v, want source and reader", gh.Capabilities)
	}
	if gh.Kinds != nil {
		t.Fatalf("a catch-all backend should report nil kinds, got %v", gh.Kinds)
	}

	relay := byName["relay"]
	if len(relay.Capabilities) != 1 || relay.Capabilities[0] != CapSource {
		t.Fatalf("relay capabilities = %v, want [source]", relay.Capabilities)
	}
	// Kinds are reported in canonical order so `gh monitor backends` is stable.
	if len(relay.Kinds) != 2 || relay.Kinds[0] != KindPR || relay.Kinds[1] != KindRun {
		t.Fatalf("relay kinds = %v, want [pr run]", relay.Kinds)
	}
}
