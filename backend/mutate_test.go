package backend

import (
	"context"
	"errors"
	"testing"
)

type stubThreads struct{ id string }

func (s *stubThreads) ListThreads(context.Context, Target, ThreadListOptions) ([]Thread, error) {
	return nil, nil
}
func (s *stubThreads) ViewThreads(context.Context, Target, []string) ([]ThreadWithComments, error) {
	return nil, nil
}
func (s *stubThreads) ResolveThread(context.Context, Target, ThreadRef) (ThreadResolution, error) {
	return ThreadResolution{}, nil
}
func (s *stubThreads) UnresolveThread(context.Context, Target, ThreadRef) (ThreadResolution, error) {
	return ThreadResolution{}, nil
}

type stubDraft struct{ id string }

func (s *stubDraft) DraftStatus(context.Context, Target, DraftRef) (DraftInfo, error) {
	return DraftInfo{}, nil
}
func (s *stubDraft) SetDraft(context.Context, Target, DraftRef, bool) (DraftResult, error) {
	return DraftResult{}, nil
}
func (s *stubDraft) ListDrafts(context.Context, Target) ([]DraftInfo, error) { return nil, nil }

func TestRegistryResolvesEachMutationCapabilityIndependently(t *testing.T) {
	reg := NewRegistry()
	builtinThreads := &stubThreads{id: "gh"}
	builtinDraft := &stubDraft{id: "gh"}
	reg.RegisterThreads("gh", nil, builtinThreads)
	reg.RegisterDraft("gh", nil, builtinDraft)

	// A backend that takes over review threads and nothing else.
	relayThreads := &stubThreads{id: "relay"}
	reg.RegisterThreads("relay", []Kind{KindPR}, relayThreads)

	got, name, err := reg.ThreadsFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("ThreadsFor: %v", err)
	}
	if got != relayThreads || name != "relay" {
		t.Fatalf("threads resolved to %q, want relay", name)
	}

	// Drafts were never claimed by relay, so they stay with the built-in one.
	// This is the property that makes a partial takeover safe.
	gotDraft, name, err := reg.DraftFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	if gotDraft != builtinDraft || name != "gh" {
		t.Fatalf("draft resolved to %q, want gh", name)
	}
}

func TestRegistryMissingMutationCapabilityIsALoudError(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterThreads("gh", nil, &stubThreads{})

	// Nothing registered a reviewer, so asking for one has to fail rather
	// than hand back a nil interface that panics at the call site.
	if _, _, err := reg.ReviewFor(Target{Kind: KindPR}); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("want ErrNoBackend, got %v", err)
	}
}

func TestRegistryMutationCapabilityRespectsKindCoverage(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterThreads("gh", nil, &stubThreads{id: "gh"})
	reg.RegisterThreads("relay", []Kind{KindPR}, &stubThreads{id: "relay"})

	if _, name, err := reg.ThreadsFor(Target{Kind: KindPR}); err != nil || name != "relay" {
		t.Fatalf("pr threads = %q, %v", name, err)
	}
	if _, name, err := reg.ThreadsFor(Target{Kind: KindIssue}); err != nil || name != "gh" {
		t.Fatalf("issue threads = %q, %v", name, err)
	}
}

func TestRegistryListIncludesMutationCapabilities(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterSource("gh", nil, &stubSource{})
	reg.RegisterThreads("gh", nil, &stubThreads{})
	reg.RegisterDraft("relay", []Kind{KindPR}, &stubDraft{})

	byName := map[string]Info{}
	for _, i := range reg.List() {
		byName[i.Name] = i
	}

	gh := byName["gh"]
	if len(gh.Capabilities) != 2 {
		t.Fatalf("gh capabilities = %v", gh.Capabilities)
	}
	// Canonical order puts source before the mutation capabilities.
	if gh.Capabilities[0] != CapSource || gh.Capabilities[1] != CapThreads {
		t.Fatalf("gh capabilities out of order: %v", gh.Capabilities)
	}

	relay := byName["relay"]
	if len(relay.Capabilities) != 1 || relay.Capabilities[0] != CapDraft {
		t.Fatalf("relay capabilities = %v, want [draft]", relay.Capabilities)
	}
}

func TestRegistryPinAppliesToMutations(t *testing.T) {
	reg := NewRegistry()
	builtin := &stubThreads{id: "gh"}
	reg.RegisterThreads("gh", nil, builtin)
	reg.RegisterThreads("relay", []Kind{KindPR}, &stubThreads{id: "relay"})

	if err := reg.Pin("gh"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got, name, err := reg.ThreadsFor(Target{Kind: KindPR})
	if err != nil {
		t.Fatalf("ThreadsFor: %v", err)
	}
	if got != builtin || name != "gh" {
		t.Fatalf("pinned threads = %q, want gh", name)
	}
}

func TestRegistryPinKnowsBackendsRegisteredOnlyForMutations(t *testing.T) {
	// A backend that provides nothing but a mutation capability still has to
	// be pinnable, or --backend would reject a name that plainly exists.
	reg := NewRegistry()
	reg.RegisterThreads("relay", nil, &stubThreads{})
	if err := reg.Pin("relay"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
}
