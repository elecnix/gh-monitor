package hub

import (
	"context"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
)

// Issue #140: a fetch whose distillation reads no per-subscriber snapshot
// options ships the distilled status, so the hub never holds the wire payload.
// `run` is the first kind to move that way; these tests pin the seam so it
// cannot silently slide back.

// TestHub_RunSubscriberReceivesTheFetchedStatus asserts the run path delivers
// the *monitor.RunStatus the fetch produced — the hub neither receives a
// payload nor re-derives the status from one.
func TestHub_RunSubscriberReceivesTheFetchedStatus(t *testing.T) {
	resp := runFixture("completed", "failure")
	h := New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return resp, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindRun), testHubOpts())
	t.Cleanup(cancelSub)

	var got *monitor.RunStatus
	for got == nil {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed before delivering a status")
			}
			st, isRun := u.Status.(*monitor.RunStatus)
			if isRun {
				got = st
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the run status")
		}
	}

	assert.Equal(t, backend.KindRun, got.TargetKind())
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, "failure", got.Conclusion)
	assert.Equal(t, 30433642, got.RunID)
	assert.Equal(t, "abcdef1", got.ShortSHA,
		"the short SHA is distilled at the fetch; the hub must not have to recompute it")
}

// TestZeroFetchedNamesWhatAFetchProduces pins, per kind, the type a handoff's
// carried Latest decodes into. `run` is the one kind whose fetch result is
// already a status; the rest still cross the boundary as wire payloads
// because their distillation genuinely varies per subscriber.
func TestZeroFetchedNamesWhatAFetchProduces(t *testing.T) {
	cases := []struct {
		kind backend.Kind
		want any
	}{
		// PR still ships the wire payload: monitor.Snapshot reads ignored
		// bots, annotation levels, the ruleset and the query tier out of
		// the subscriber's options, so the hub must hold the payload.
		{backend.KindPR, &monitor.PullRequest{}},
		{backend.KindRef, &monitor.RefQueryResponse{}},
		{backend.KindCommit, &monitor.CommitQueryResponse{}},
		{backend.KindIssue, &monitor.IssueQueryResponse{}},
		{backend.KindRepo, &monitor.RepoQueryResponse{}},
		// run distills at the fetch, so what crosses the boundary is the
		// status — the payload never leaves internal/monitor.
		{backend.KindRun, &monitor.RunStatus{}},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			assert.IsType(t, c.want, zeroFetched(c.kind))
		})
	}
}

// TestZeroFetchedRunMatchesTheStatusTable keeps the two per-kind tables honest
// with each other: once a kind's fetch result IS its status, the handoff has
// one zero value to name, not two.
func TestZeroFetchedRunMatchesTheStatusTable(t *testing.T) {
	assert.IsType(t, newStatusForKind(backend.KindRun), zeroFetched(backend.KindRun))
}