package gh

import (
	"context"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/comments"
	"github.com/elecnix/gh-monitor/internal/draft"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/reactions"
	"github.com/elecnix/gh-monitor/internal/review"
	"github.com/elecnix/gh-monitor/internal/threads"
)

// The mutation capabilities, each backed by the service that already knows the
// GraphQL for it. The adapters carry no logic of their own: they convert a
// Target to the identity those services take, and hand the call over.
//
// The context is accepted and not used. These services call the gh CLI
// synchronously and cannot be cancelled mid-flight; taking a context anyway
// keeps the interface honest for a backend that can.

// threadActor implements backend.ThreadActor.
type threadActor struct{ p *Provider }

func (a threadActor) service(t backend.Target) *threads.Service {
	return threads.NewService(a.p.api(t.Host))
}

func (a threadActor) ListThreads(_ context.Context, t backend.Target, opts backend.ThreadListOptions) ([]backend.Thread, error) {
	return a.service(t).List(monitor.IdentityOf(t), opts)
}

func (a threadActor) ViewThreads(_ context.Context, t backend.Target, ids []string) ([]backend.ThreadWithComments, error) {
	return a.service(t).GetThreadsByID(ids)
}

func (a threadActor) ResolveThread(_ context.Context, t backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	return a.service(t).Resolve(monitor.IdentityOf(t), ref)
}

func (a threadActor) UnresolveThread(_ context.Context, t backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	return a.service(t).Unresolve(monitor.IdentityOf(t), ref)
}

// reviewActor implements backend.ReviewActor.
type reviewActor struct{ p *Provider }

func (a reviewActor) service(t backend.Target) *review.Service {
	return review.NewService(a.p.api(t.Host))
}

func (a reviewActor) StartReview(_ context.Context, t backend.Target, commitOID string) (*backend.ReviewState, error) {
	return a.service(t).Start(monitor.IdentityOf(t), commitOID)
}

func (a reviewActor) AddReviewComment(_ context.Context, t backend.Target, in backend.ReviewCommentInput) (*backend.ReviewThread, error) {
	return a.service(t).AddThread(monitor.IdentityOf(t), in)
}

func (a reviewActor) UpdateReviewComment(_ context.Context, t backend.Target, in backend.ReviewCommentUpdate) error {
	return a.service(t).UpdateComment(monitor.IdentityOf(t), in)
}

func (a reviewActor) DeleteReviewComment(_ context.Context, t backend.Target, in backend.ReviewCommentDelete) error {
	return a.service(t).DeleteComment(monitor.IdentityOf(t), in)
}

func (a reviewActor) SubmitReview(_ context.Context, t backend.Target, in backend.ReviewSubmitInput) (*backend.ReviewSubmitStatus, error) {
	return a.service(t).Submit(monitor.IdentityOf(t), in)
}

// commentActor implements backend.CommentActor.
type commentActor struct{ p *Provider }

func (a commentActor) ReplyToThread(_ context.Context, t backend.Target, opts backend.ReplyOptions) (backend.Reply, error) {
	return comments.NewService(a.p.api(t.Host)).Reply(monitor.IdentityOf(t), opts)
}

// draftActor implements backend.DraftActor.
type draftActor struct{ p *Provider }

func (a draftActor) service(t backend.Target) *draft.Service {
	return draft.NewService(a.p.api(t.Host))
}

func (a draftActor) DraftStatus(_ context.Context, t backend.Target, ref backend.DraftRef) (backend.DraftInfo, error) {
	return a.service(t).Status(monitor.IdentityOf(t), ref)
}

func (a draftActor) SetDraft(_ context.Context, t backend.Target, ref backend.DraftRef, isDraft bool) (backend.DraftResult, error) {
	svc := a.service(t)
	if isDraft {
		return svc.Draft(monitor.IdentityOf(t), ref)
	}
	return svc.Ready(monitor.IdentityOf(t), ref)
}

func (a draftActor) ListDrafts(_ context.Context, t backend.Target) ([]backend.DraftInfo, error) {
	return a.service(t).List(monitor.IdentityOf(t))
}

// reactionActor implements backend.ReactionActor.
type reactionActor struct{ p *Provider }

func (a reactionActor) React(_ context.Context, t backend.Target, subjectID, reaction string) error {
	return reactions.React(a.p.api(t.Host), subjectID, reaction)
}
