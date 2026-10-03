package remote

import (
	"context"
	"sort"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
)

// fakeActors implements every mutation capability. It exists so a
// ServerConfig can be populated without pulling in five separate fakes; the
// per-capability behaviour is covered by mutate_test.go.
type fakeActors struct{}

func (*fakeActors) ListThreads(context.Context, backend.Target, backend.ThreadListOptions) ([]backend.Thread, error) {
	return nil, nil
}

func (*fakeActors) ViewThreads(context.Context, backend.Target, []string) ([]backend.ThreadWithComments, error) {
	return nil, nil
}

func (*fakeActors) ResolveThread(context.Context, backend.Target, backend.ThreadRef) (backend.ThreadResolution, error) {
	return backend.ThreadResolution{}, nil
}

func (*fakeActors) UnresolveThread(context.Context, backend.Target, backend.ThreadRef) (backend.ThreadResolution, error) {
	return backend.ThreadResolution{}, nil
}

func (*fakeActors) StartReview(context.Context, backend.Target, string) (*backend.ReviewState, error) {
	return nil, nil
}

func (*fakeActors) AddReviewComment(context.Context, backend.Target, backend.ReviewCommentInput) (*backend.ReviewThread, error) {
	return nil, nil
}

func (*fakeActors) UpdateReviewComment(context.Context, backend.Target, backend.ReviewCommentUpdate) error {
	return nil
}

func (*fakeActors) DeleteReviewComment(context.Context, backend.Target, backend.ReviewCommentDelete) error {
	return nil
}

func (*fakeActors) SubmitReview(context.Context, backend.Target, backend.ReviewSubmitInput) (*backend.ReviewSubmitStatus, error) {
	return nil, nil
}

func (*fakeActors) ReplyToThread(context.Context, backend.Target, backend.ReplyOptions) (backend.Reply, error) {
	return backend.Reply{}, nil
}

func (*fakeActors) DraftStatus(context.Context, backend.Target, backend.DraftRef) (backend.DraftInfo, error) {
	return backend.DraftInfo{}, nil
}

func (*fakeActors) SetDraft(context.Context, backend.Target, backend.DraftRef, bool) (backend.DraftResult, error) {
	return backend.DraftResult{}, nil
}

func (*fakeActors) ListDrafts(context.Context, backend.Target) ([]backend.DraftInfo, error) {
	return nil, nil
}

func (*fakeActors) React(context.Context, backend.Target, string, string) error { return nil }

// setServerActor points cfg at one capability and leaves the rest nil, so a
// test can assert the announced surface is exactly that one capability.
func setServerActor(t *testing.T, cfg *ServerConfig, c backend.Capability) {
	t.Helper()
	switch c {
	case backend.CapSource:
		cfg.Source = &fakeBackend{}
	case backend.CapReader:
		cfg.Reader = &fakeBackend{}
	case backend.CapThreads:
		cfg.Threads = &fakeActors{}
	case backend.CapReview:
		cfg.Review = &fakeActors{}
	case backend.CapComments:
		cfg.Comments = &fakeActors{}
	case backend.CapDraft:
		cfg.Draft = &fakeActors{}
	case backend.CapReactions:
		cfg.Reactions = &fakeActors{}
	default:
		t.Fatalf("no ServerConfig field for capability %q", c)
	}
}

// TestCapabilityBindingsCoverEveryCapability is the point of this table. A
// capability added to backend's descriptor and to ServerConfig but forgotten
// here used to validate cleanly, register nothing, and read as a backend that
// was present and silent. Now the omission fails the build.
func TestCapabilityBindingsCoverEveryCapability(t *testing.T) {
	all := backend.AllCapabilities()

	if len(providerBindings) != len(all) {
		var have []string
		for c := range providerBindings {
			have = append(have, string(c))
		}
		sort.Strings(have)
		t.Fatalf("providerBindings has %d entries %v, want one per capability %v",
			len(providerBindings), have, all)
	}
	for _, c := range all {
		b, ok := providerBindings[c]
		if !ok {
			t.Errorf("no remote binding for capability %q", c)
			continue
		}
		if b.server == nil || b.client == nil || b.register == nil {
			t.Errorf("binding for %q is incomplete: %+v", c, b)
		}
	}
	for c := range providerBindings {
		if !backend.IsMutation(c) && c != backend.CapSource && c != backend.CapReader {
			t.Errorf("providerBindings declares %q, which no capability does", c)
		}
	}
}

// TestServerConfigCapabilitiesFollowsTheDescriptor checks that what a server
// announces is read from the descriptor rather than written out by hand, in
// canonical order. The empty case is covered explicitly: a configuration with
// no implementation of anything announces nothing, which Serve rejects rather
// than advertising a backend that cannot do anything.
func TestServerConfigCapabilitiesFollowsTheDescriptor(t *testing.T) {
	empty, err := ServerConfig{Name: "empty"}.capabilities()
	if err != nil {
		t.Fatalf("capabilities() error = %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an empty ServerConfig announces %v, want none", empty)
	}

	// One field set at a time: each must announce exactly that capability and
	// nothing else.
	all := backend.AllCapabilities()
	for _, name := range all {
		cfg := ServerConfig{Name: "one"}
		setServerActor(t, &cfg, name)

		got, err := cfg.capabilities()
		if err != nil {
			t.Fatalf("%s: capabilities() error = %v", name, err)
		}
		if len(got) != 1 || got[0] != name {
			t.Errorf("a ServerConfig serving only %q announces %v", name, got)
		}
	}

	// All fields set: canonical order, matching AllCapabilities.
	full := ServerConfig{
		Name:      "full",
		Source:    &fakeBackend{},
		Reader:    &fakeBackend{},
		Threads:   &fakeActors{},
		Review:    &fakeActors{},
		Comments:  &fakeActors{},
		Draft:     &fakeActors{},
		Reactions: &fakeActors{},
	}
	got, err := full.capabilities()
	if err != nil {
		t.Fatalf("capabilities() error = %v", err)
	}
	if len(got) != len(all) {
		t.Fatalf("a fully populated ServerConfig announces %v, want every capability", got)
	}
	for i := range all {
		if got[i] != all[i] {
			t.Fatalf("a fully populated ServerConfig announces %v, want %v", got, all)
		}
	}
}

// TestProviderRegistersExactlyWhatTheServerAnnounced is the end-to-end proof
// that the binding table connects both ends: what a ServerConfig serves is
// what the client ends up able to resolve. A capability served but not
// registered would show up here as a ResolveCapability error.
func TestProviderRegistersExactlyWhatTheServerAnnounced(t *testing.T) {
	ctx := context.Background()
	tr := pipeTransport(t, ServerConfig{
		Name:      "relay",
		Kinds:     []backend.Kind{backend.KindPR},
		Reader:    &fakeBackend{},
		Draft:     &fakeActors{},
		Reactions: &fakeActors{},
	})
	p, err := Connect(ctx, tr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	r := backend.NewRegistry()
	if err := p.Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, info := range r.List() {
		want := []backend.Capability{backend.CapReader, backend.CapDraft, backend.CapReactions}
		if len(info.Capabilities) != len(want) {
			t.Errorf("relay registered %v, want exactly %v", info.Capabilities, want)
			continue
		}
		for i := range want {
			if info.Capabilities[i] != want[i] {
				t.Errorf("relay registered %v, want exactly %v", info.Capabilities, want)
				break
			}
		}
	}

	tgt := backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}
	for _, c := range []backend.Capability{backend.CapReader, backend.CapDraft, backend.CapReactions} {
		if _, name, err := r.ResolveCapability(c, tgt); err != nil {
			t.Errorf("ResolveCapability(%s) error = %v", c, err)
		} else if name != "relay" {
			t.Errorf("ResolveCapability(%s) name = %q, want relay", c, name)
		}
	}
	// The server said nothing about threads or source, so nothing resolves
	// them: one declared capability must not stand in for another.
	for _, c := range []backend.Capability{backend.CapThreads, backend.CapSource, backend.CapReview, backend.CapComments} {
		if _, _, err := r.ResolveCapability(c, tgt); err == nil {
			t.Errorf("ResolveCapability(%s) = nil error, want ErrNoBackend for a capability the server did not declare", c)
		}
	}
}

// TestValidateHelloRejectsUnknownCapability checks the descriptor is what
// decides what a server may declare, so the wire and the registry agree on
// the vocabulary.
func TestValidateHelloRejectsUnknownCapability(t *testing.T) {
	ok := Hello{Protocol: Protocol, Name: "relay", Capabilities: []backend.Capability{backend.CapThreads}}
	if err := validateHello(ok); err != nil {
		t.Fatalf("validateHello(threads) = %v, want nil", err)
	}
	bad := Hello{Protocol: Protocol, Name: "relay", Capabilities: []backend.Capability{"checks"}}
	if err := validateHello(bad); err == nil {
		t.Error("validateHello(checks) = nil, want unknown capability")
	}
}
