package remote

import (
	"context"
	"errors"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
)

// recordingThreads captures what it was asked, and answers with fixtures.
type recordingThreads struct {
	listOpts backend.ThreadListOptions
	viewIDs  []string
	ref      backend.ThreadRef
	resolved bool
	err      error
}

func (r *recordingThreads) ListThreads(_ context.Context, _ backend.Target, opts backend.ThreadListOptions) ([]backend.Thread, error) {
	r.listOpts = opts
	if r.err != nil {
		return nil, r.err
	}
	return []backend.Thread{{ThreadID: "PRRT_1", IsResolved: false, Path: "a.go"}}, nil
}

func (r *recordingThreads) ViewThreads(_ context.Context, _ backend.Target, ids []string) ([]backend.ThreadWithComments, error) {
	r.viewIDs = ids
	return []backend.ThreadWithComments{{
		ThreadID: ids[0],
		Comments: []backend.ThreadComment{{ID: "c1", Body: "hi", Author: "someone"}},
	}}, nil
}

func (r *recordingThreads) ResolveThread(_ context.Context, _ backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	r.ref, r.resolved = ref, true
	return backend.ThreadResolution{ThreadNodeID: ref.ThreadID, IsResolved: true}, nil
}

func (r *recordingThreads) UnresolveThread(_ context.Context, _ backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	r.ref, r.resolved = ref, false
	return backend.ThreadResolution{ThreadNodeID: ref.ThreadID, IsResolved: false}, nil
}

type recordingDraft struct {
	ref   backend.DraftRef
	draft bool
}

func (d *recordingDraft) DraftStatus(_ context.Context, _ backend.Target, ref backend.DraftRef) (backend.DraftInfo, error) {
	d.ref = ref
	return backend.DraftInfo{PRNumber: ref.PRNumber, IsDraft: true, Title: "wip"}, nil
}

func (d *recordingDraft) SetDraft(_ context.Context, _ backend.Target, ref backend.DraftRef, draft bool) (backend.DraftResult, error) {
	d.ref, d.draft = ref, draft
	return backend.DraftResult{PRNumber: ref.PRNumber, IsDraft: draft, Status: "ok"}, nil
}

func (d *recordingDraft) ListDrafts(_ context.Context, _ backend.Target) ([]backend.DraftInfo, error) {
	return []backend.DraftInfo{{PRNumber: 1, IsDraft: true, Title: "one"}}, nil
}

type recordingReactions struct {
	subjectID string
	reaction  string
}

func (r *recordingReactions) React(_ context.Context, _ backend.Target, subjectID, reaction string) error {
	r.subjectID, r.reaction = subjectID, reaction
	return nil
}

func TestThreadMutationsRoundTripAcrossTheWire(t *testing.T) {
	actor := &recordingThreads{}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Threads: actor,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	list, err := p.ListThreads(context.Background(), prTarget(), backend.ThreadListOptions{OnlyUnresolved: true})
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if !actor.listOpts.OnlyUnresolved {
		t.Fatal("the filter did not survive the wire")
	}
	if len(list) != 1 || list[0].ThreadID != "PRRT_1" || list[0].Path != "a.go" {
		t.Fatalf("ListThreads returned %+v", list)
	}

	viewed, err := p.ViewThreads(context.Background(), prTarget(), []string{"PRRT_9"})
	if err != nil {
		t.Fatalf("ViewThreads: %v", err)
	}
	if len(actor.viewIDs) != 1 || actor.viewIDs[0] != "PRRT_9" {
		t.Fatalf("server saw ids %v", actor.viewIDs)
	}
	if len(viewed) != 1 || len(viewed[0].Comments) != 1 || viewed[0].Comments[0].Body != "hi" {
		t.Fatalf("ViewThreads returned %+v", viewed)
	}

	res, err := p.ResolveThread(context.Background(), prTarget(), backend.ThreadRef{ThreadID: "PRRT_2"})
	if err != nil {
		t.Fatalf("ResolveThread: %v", err)
	}
	if !actor.resolved || actor.ref.ThreadID != "PRRT_2" || !res.IsResolved {
		t.Fatalf("resolve did not round trip: %+v %+v", actor.ref, res)
	}

	if _, err := p.UnresolveThread(context.Background(), prTarget(), backend.ThreadRef{ThreadID: "PRRT_3"}); err != nil {
		t.Fatalf("UnresolveThread: %v", err)
	}
	if actor.resolved || actor.ref.ThreadID != "PRRT_3" {
		t.Fatalf("unresolve did not round trip: %+v", actor.ref)
	}
}

func TestDraftMutationsRoundTripAcrossTheWire(t *testing.T) {
	actor := &recordingDraft{}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Draft: actor,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	info, err := p.DraftStatus(context.Background(), prTarget(), backend.DraftRef{PRNumber: 42})
	if err != nil {
		t.Fatalf("DraftStatus: %v", err)
	}
	if info.PRNumber != 42 || !info.IsDraft || info.Title != "wip" {
		t.Fatalf("DraftStatus returned %+v", info)
	}

	result, err := p.SetDraft(context.Background(), prTarget(), backend.DraftRef{PRNumber: 42}, false)
	if err != nil {
		t.Fatalf("SetDraft: %v", err)
	}
	if actor.draft {
		t.Fatal("the draft flag did not survive the wire")
	}
	if result.Status != "ok" || result.IsDraft {
		t.Fatalf("SetDraft returned %+v", result)
	}

	all, err := p.ListDrafts(context.Background(), prTarget())
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	if len(all) != 1 || all[0].Title != "one" {
		t.Fatalf("ListDrafts returned %+v", all)
	}
}

func TestReactionRoundTripsAcrossTheWire(t *testing.T) {
	actor := &recordingReactions{}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Reactions: actor,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := p.React(context.Background(), prTarget(), "PRRC_1", "THUMBS_UP"); err != nil {
		t.Fatalf("React: %v", err)
	}
	if actor.subjectID != "PRRC_1" || actor.reaction != "THUMBS_UP" {
		t.Fatalf("server saw %q %q", actor.subjectID, actor.reaction)
	}
}

func TestMutationErrorsCrossTheWire(t *testing.T) {
	actor := &recordingThreads{err: errors.New("upstream said no")}
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Threads: actor,
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = p.ListThreads(context.Background(), prTarget(), backend.ThreadListOptions{})
	if err == nil {
		t.Fatal("a failing mutation must surface as an error, not an empty result")
	}
	if !contains(err.Error(), "upstream said no") {
		t.Fatalf("error lost its reason: %v", err)
	}
}

func TestCallingAnUnservedMutationFails(t *testing.T) {
	// The server declares threads only; asking it to submit a review must
	// fail clearly rather than hang or return a zero value.
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name: "relay", Kinds: []backend.Kind{backend.KindPR}, Threads: &recordingThreads{},
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = p.SubmitReview(context.Background(), prTarget(), backend.ReviewSubmitInput{ReviewID: "PRR_1"})
	if err == nil {
		t.Fatal("submitting to a backend without the review capability must fail")
	}
}

func TestServerDeclaresEveryConfiguredMutationCapability(t *testing.T) {
	p, err := Connect(context.Background(), pipeTransport(t, ServerConfig{
		Name:      "relay",
		Kinds:     []backend.Kind{backend.KindPR},
		Threads:   &recordingThreads{},
		Draft:     &recordingDraft{},
		Reactions: &recordingReactions{},
	}))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	reg := backend.NewRegistry()
	reg.RegisterThreads("gh", nil, &recordingThreads{})
	reg.RegisterReview("gh", nil, stubReview{})
	reg.RegisterDraft("gh", nil, &recordingDraft{})
	if err := reg.Use(p); err != nil {
		t.Fatalf("Use: %v", err)
	}

	if _, name, err := reg.ThreadsFor(prTarget()); err != nil || name != "relay" {
		t.Fatalf("threads = %q, %v", name, err)
	}
	if _, name, err := reg.DraftFor(prTarget()); err != nil || name != "relay" {
		t.Fatalf("draft = %q, %v", name, err)
	}
	// Review was never declared, so it stays with the built-in backend.
	if _, name, err := reg.ReviewFor(prTarget()); err != nil || name != "gh" {
		t.Fatalf("review = %q, %v", name, err)
	}
}

// stubReview stands in for the built-in review capability.
type stubReview struct{}

func (stubReview) StartReview(context.Context, backend.Target, string) (*backend.ReviewState, error) {
	return nil, nil
}
func (stubReview) AddReviewComment(context.Context, backend.Target, backend.ReviewCommentInput) (*backend.ReviewThread, error) {
	return nil, nil
}
func (stubReview) UpdateReviewComment(context.Context, backend.Target, backend.ReviewCommentUpdate) error {
	return nil
}
func (stubReview) DeleteReviewComment(context.Context, backend.Target, backend.ReviewCommentDelete) error {
	return nil
}
func (stubReview) SubmitReview(context.Context, backend.Target, backend.ReviewSubmitInput) (*backend.ReviewSubmitStatus, error) {
	return nil, nil
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
