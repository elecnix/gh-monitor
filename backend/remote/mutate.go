package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"

	"github.com/elecnix/gh-monitor/backend"
)

// The mutation operations. Each maps to one method on one capability
// interface; the payload and result are that method's arguments and return
// value, encoded as JSON.
const (
	OpThreadsList      = "threads.list"
	OpThreadsView      = "threads.view"
	OpThreadsResolve   = "threads.resolve"
	OpThreadsUnresolve = "threads.unresolve"

	OpReviewStart         = "review.start"
	OpReviewAddComment    = "review.addComment"
	OpReviewUpdateComment = "review.updateComment"
	OpReviewDeleteComment = "review.deleteComment"
	OpReviewSubmit        = "review.submit"

	OpCommentsReply = "comments.reply"

	OpDraftStatus = "draft.status"
	OpDraftSet    = "draft.set"
	OpDraftList   = "draft.list"

	OpReactionsReact = "reactions.react"
)

// Payloads for the operations whose arguments are more than a Target.

type threadViewPayload struct {
	ThreadIDs []string `json:"thread_ids"`
}

type reviewStartPayload struct {
	CommitOID string `json:"commit_oid"`
}

type draftSetPayload struct {
	Ref   backend.DraftRef `json:"ref"`
	Draft bool             `json:"draft"`
}

type reactPayload struct {
	SubjectID string `json:"subject_id"`
	Reaction  string `json:"reaction"`
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// call performs one request/response exchange and decodes the result.
func call[Out any](ctx context.Context, p *Provider, op string, t backend.Target, payload any) (Out, error) {
	var out Out

	conn, err := p.transport.Open(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	if _, err := readHello(ctx, conn, br); err != nil {
		return out, fmt.Errorf("read hello from %s: %w", p.transport, err)
	}

	req := Request{Op: op, Target: t}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return out, fmt.Errorf("encode %s payload: %w", op, err)
		}
		req.Payload = raw
	}
	if err := writeJSON(conn, req); err != nil {
		return out, fmt.Errorf("send %s to %s: %w", op, p.transport, err)
	}

	var frame Frame
	if err := readJSON(br, &frame); err != nil {
		return out, fmt.Errorf("read %s response from %s: %w", op, p.transport, err)
	}
	if frame.Error != "" {
		return out, fmt.Errorf("%s: %s", p.hello.Name, frame.Error)
	}
	if len(frame.Result) == 0 || string(frame.Result) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(frame.Result, &out); err != nil {
		return out, fmt.Errorf("decode %s result: %w", op, err)
	}
	return out, nil
}

// ListThreads implements backend.ThreadActor.
func (p *Provider) ListThreads(ctx context.Context, t backend.Target, opts backend.ThreadListOptions) ([]backend.Thread, error) {
	return call[[]backend.Thread](ctx, p, OpThreadsList, t, opts)
}

// ViewThreads implements backend.ThreadActor.
func (p *Provider) ViewThreads(ctx context.Context, t backend.Target, ids []string) ([]backend.ThreadWithComments, error) {
	return call[[]backend.ThreadWithComments](ctx, p, OpThreadsView, t, threadViewPayload{ThreadIDs: ids})
}

// ResolveThread implements backend.ThreadActor.
func (p *Provider) ResolveThread(ctx context.Context, t backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	return call[backend.ThreadResolution](ctx, p, OpThreadsResolve, t, ref)
}

// UnresolveThread implements backend.ThreadActor.
func (p *Provider) UnresolveThread(ctx context.Context, t backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	return call[backend.ThreadResolution](ctx, p, OpThreadsUnresolve, t, ref)
}

// StartReview implements backend.ReviewActor.
func (p *Provider) StartReview(ctx context.Context, t backend.Target, commitOID string) (*backend.ReviewState, error) {
	return call[*backend.ReviewState](ctx, p, OpReviewStart, t, reviewStartPayload{CommitOID: commitOID})
}

// AddReviewComment implements backend.ReviewActor.
func (p *Provider) AddReviewComment(ctx context.Context, t backend.Target, in backend.ReviewCommentInput) (*backend.ReviewThread, error) {
	return call[*backend.ReviewThread](ctx, p, OpReviewAddComment, t, in)
}

// UpdateReviewComment implements backend.ReviewActor.
func (p *Provider) UpdateReviewComment(ctx context.Context, t backend.Target, in backend.ReviewCommentUpdate) error {
	_, err := call[struct{}](ctx, p, OpReviewUpdateComment, t, in)
	return err
}

// DeleteReviewComment implements backend.ReviewActor.
func (p *Provider) DeleteReviewComment(ctx context.Context, t backend.Target, in backend.ReviewCommentDelete) error {
	_, err := call[struct{}](ctx, p, OpReviewDeleteComment, t, in)
	return err
}

// SubmitReview implements backend.ReviewActor.
func (p *Provider) SubmitReview(ctx context.Context, t backend.Target, in backend.ReviewSubmitInput) (*backend.ReviewSubmitStatus, error) {
	return call[*backend.ReviewSubmitStatus](ctx, p, OpReviewSubmit, t, in)
}

// ReplyToThread implements backend.CommentActor.
func (p *Provider) ReplyToThread(ctx context.Context, t backend.Target, opts backend.ReplyOptions) (backend.Reply, error) {
	return call[backend.Reply](ctx, p, OpCommentsReply, t, opts)
}

// DraftStatus implements backend.DraftActor.
func (p *Provider) DraftStatus(ctx context.Context, t backend.Target, ref backend.DraftRef) (backend.DraftInfo, error) {
	return call[backend.DraftInfo](ctx, p, OpDraftStatus, t, ref)
}

// SetDraft implements backend.DraftActor.
func (p *Provider) SetDraft(ctx context.Context, t backend.Target, ref backend.DraftRef, draft bool) (backend.DraftResult, error) {
	return call[backend.DraftResult](ctx, p, OpDraftSet, t, draftSetPayload{Ref: ref, Draft: draft})
}

// ListDrafts implements backend.DraftActor.
func (p *Provider) ListDrafts(ctx context.Context, t backend.Target) ([]backend.DraftInfo, error) {
	return call[[]backend.DraftInfo](ctx, p, OpDraftList, t, nil)
}

// React implements backend.ReactionActor.
func (p *Provider) React(ctx context.Context, t backend.Target, subjectID, reaction string) error {
	_, err := call[struct{}](ctx, p, OpReactionsReact, t, reactPayload{SubjectID: subjectID, Reaction: reaction})
	return err
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// serveMutation dispatches one mutation op to the configured actor. It reports
// whether op was a mutation at all, so Serve can fall through to watch/read.
func serveMutation(ctx context.Context, conn interface{ Write([]byte) (int, error) }, cfg ServerConfig, req Request) (handled bool, err error) {
	decode := func(v any) error {
		if len(req.Payload) == 0 {
			return nil
		}
		return json.Unmarshal(req.Payload, v)
	}
	// respond marshals a result, or reports the failure the actor returned.
	// Either way the client gets exactly one frame.
	respond := func(result any, actorErr error) error {
		if actorErr != nil {
			return writeJSON(conn, Frame{Error: actorErr.Error()})
		}
		raw, mErr := json.Marshal(result)
		if mErr != nil {
			return writeJSON(conn, Frame{Error: fmt.Sprintf("encode result: %v", mErr)})
		}
		return writeJSON(conn, Frame{Result: raw})
	}
	missing := func(capability backend.Capability) error {
		return writeJSON(conn, Frame{Error: fmt.Sprintf("backend %q does not provide %s", cfg.Name, capability)})
	}

	switch req.Op {
	case OpThreadsList:
		if cfg.Threads == nil {
			return true, missing(backend.CapThreads)
		}
		var opts backend.ThreadListOptions
		if err := decode(&opts); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Threads.ListThreads(ctx, req.Target, opts)
		return true, respond(out, aErr)

	case OpThreadsView:
		if cfg.Threads == nil {
			return true, missing(backend.CapThreads)
		}
		var in threadViewPayload
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Threads.ViewThreads(ctx, req.Target, in.ThreadIDs)
		return true, respond(out, aErr)

	case OpThreadsResolve, OpThreadsUnresolve:
		if cfg.Threads == nil {
			return true, missing(backend.CapThreads)
		}
		var ref backend.ThreadRef
		if err := decode(&ref); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		var out backend.ThreadResolution
		var aErr error
		if req.Op == OpThreadsResolve {
			out, aErr = cfg.Threads.ResolveThread(ctx, req.Target, ref)
		} else {
			out, aErr = cfg.Threads.UnresolveThread(ctx, req.Target, ref)
		}
		return true, respond(out, aErr)

	case OpReviewStart:
		if cfg.Review == nil {
			return true, missing(backend.CapReview)
		}
		var in reviewStartPayload
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Review.StartReview(ctx, req.Target, in.CommitOID)
		return true, respond(out, aErr)

	case OpReviewAddComment:
		if cfg.Review == nil {
			return true, missing(backend.CapReview)
		}
		var in backend.ReviewCommentInput
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Review.AddReviewComment(ctx, req.Target, in)
		return true, respond(out, aErr)

	case OpReviewUpdateComment:
		if cfg.Review == nil {
			return true, missing(backend.CapReview)
		}
		var in backend.ReviewCommentUpdate
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		return true, respond(struct{}{}, cfg.Review.UpdateReviewComment(ctx, req.Target, in))

	case OpReviewDeleteComment:
		if cfg.Review == nil {
			return true, missing(backend.CapReview)
		}
		var in backend.ReviewCommentDelete
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		return true, respond(struct{}{}, cfg.Review.DeleteReviewComment(ctx, req.Target, in))

	case OpReviewSubmit:
		if cfg.Review == nil {
			return true, missing(backend.CapReview)
		}
		var in backend.ReviewSubmitInput
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Review.SubmitReview(ctx, req.Target, in)
		return true, respond(out, aErr)

	case OpCommentsReply:
		if cfg.Comments == nil {
			return true, missing(backend.CapComments)
		}
		var in backend.ReplyOptions
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Comments.ReplyToThread(ctx, req.Target, in)
		return true, respond(out, aErr)

	case OpDraftStatus:
		if cfg.Draft == nil {
			return true, missing(backend.CapDraft)
		}
		var ref backend.DraftRef
		if err := decode(&ref); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Draft.DraftStatus(ctx, req.Target, ref)
		return true, respond(out, aErr)

	case OpDraftSet:
		if cfg.Draft == nil {
			return true, missing(backend.CapDraft)
		}
		var in draftSetPayload
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		out, aErr := cfg.Draft.SetDraft(ctx, req.Target, in.Ref, in.Draft)
		return true, respond(out, aErr)

	case OpDraftList:
		if cfg.Draft == nil {
			return true, missing(backend.CapDraft)
		}
		out, aErr := cfg.Draft.ListDrafts(ctx, req.Target)
		return true, respond(out, aErr)

	case OpReactionsReact:
		if cfg.Reactions == nil {
			return true, missing(backend.CapReactions)
		}
		var in reactPayload
		if err := decode(&in); err != nil {
			return true, writeJSON(conn, Frame{Error: err.Error()})
		}
		return true, respond(struct{}{}, cfg.Reactions.React(ctx, req.Target, in.SubjectID, in.Reaction))
	}

	return false, nil
}
