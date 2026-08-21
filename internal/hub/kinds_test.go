package hub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Non-PR fixtures. Each mirrors what the corresponding monitor.Service fetch
// returns, and each uses terminal states the API actually reports (see
// AGENTS.md: a blank status/conclusion does not exist).

func refFixture(oid string, failing []string) *monitor.RefQueryResponse {
	var resp monitor.RefQueryResponse
	runs := make([]monitor.CheckRun, 0, len(failing))
	for _, name := range failing {
		runs = append(runs, monitor.CheckRun{Name: name, Conclusion: "FAILURE"})
	}
	suite := monitor.CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: monitor.AppInfo{Name: "CI"}, CheckRuns: monitor.RunNodes{Nodes: runs}}
	if len(failing) > 0 {
		suite.Conclusion = ""
	}
	resp.Repository.Ref = &monitor.RefTarget{}
	resp.Repository.Ref.Target.Oid = oid
	resp.Repository.Ref.Target.CheckSuites = monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}}
	return &resp
}

func issueFixture(state string, commentIDs ...string) *monitor.IssueQueryResponse {
	var resp monitor.IssueQueryResponse
	node := &monitor.IssueNode{State: state}
	for _, id := range commentIDs {
		node.Comments.Nodes = append(node.Comments.Nodes, monitor.IssueComment{ID: id})
	}
	resp.Repository.Issue = node
	return &resp
}

func runFixture(status, conclusion string) *monitor.WorkflowRun {
	return &monitor.WorkflowRun{
		ID:         30433642,
		Status:     status,
		Conclusion: conclusion,
		HTMLURL:    "https://github.com/octo/demo/actions/runs/30433642",
		RunNumber:  42,
	}
}

func repoFixture(prNumbers ...int) *monitor.RepoQueryResponse {
	var resp monitor.RepoQueryResponse
	for _, n := range prNumbers {
		resp.Repository.PullRequests.Nodes = append(resp.Repository.PullRequests.Nodes,
			monitor.RepoPR{Number: n, Title: "PR", State: "OPEN", URL: "u", CreatedAt: "2026-08-21T00:00:00Z"})
	}
	return &resp
}

func targetOf(kind backend.Kind) backend.Target {
	t := backend.Target{Kind: kind, Owner: "o", Repo: "r", Host: "github.com"}
	switch kind {
	case backend.KindRef:
		t.Ref = "main"
	case backend.KindIssue:
		t.Number = 5
	}
	return t
}

// waitClosed reads updates until the channel closes, returning event types.
func waitClosed(t *testing.T, ch <-chan backend.Update) []string {
	t.Helper()
	var out []string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, string(u.Event.Type))
		case <-deadline:
			t.Fatal("timed out waiting for the subscription to close")
		}
	}
}

func TestHub_SubscribeRef(t *testing.T) {
	var resp *monitor.RefQueryResponse
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return resp, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resp = refFixture("aaaaaaa", []string{"lint"})
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindRef), testHubOpts())
	t.Cleanup(cancelSub)

	// First poll surfaces the pre-existing failure against the empty baseline.
	got := collect(ch, 100*time.Millisecond)
	assert.Contains(t, got, "first-poll")
	assert.Contains(t, got, "new-failing-checks")

	// A changed head commit surfaces as new-commit.
	resp = refFixture("bbbbbbb", []string{"lint"})
	require.NoError(t, h.Refresh(monitor.IdentityOf(targetOf(backend.KindRef))))
	got = collect(ch, 100*time.Millisecond)
	assert.Contains(t, got, "new-commit")
}

func TestHub_SubscribeIssue(t *testing.T) {
	var resp *monitor.IssueQueryResponse
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return resp, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resp = issueFixture("OPEN", "c1")
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindIssue), testHubOpts())
	t.Cleanup(cancelSub)

	got := collect(ch, 100*time.Millisecond)
	assert.Contains(t, got, "first-poll")
	assert.Contains(t, got, "issue-new-comment")

	// Closing the issue is terminal: the subscription closes cleanly after
	// the issue-closed event, so daemon clients get a clean EOF.
	resp = issueFixture("CLOSED", "c1")
	require.NoError(t, h.Refresh(monitor.IdentityOf(targetOf(backend.KindIssue))))
	types := waitClosed(t, ch)
	assert.Contains(t, types, "issue-closed")
}

func TestHub_SubscribeRun(t *testing.T) {
	var resp *monitor.WorkflowRun
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return resp, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resp = runFixture("in_progress", "")
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindRun), testHubOpts())
	t.Cleanup(cancelSub)

	got := collect(ch, 100*time.Millisecond)
	assert.Contains(t, got, "first-poll")

	// Completion is terminal: run-completed, then a clean channel close.
	resp = runFixture("completed", "success")
	require.NoError(t, h.Refresh(monitor.IdentityOf(targetOf(backend.KindRun))))
	types := waitClosed(t, ch)
	assert.Contains(t, types, "run-completed")
}

func TestHub_SubscribeRepo(t *testing.T) {
	var resp *monitor.RepoQueryResponse
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return resp, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resp = repoFixture(1)
	opts := testHubOpts()
	opts.Since = "" // no cursor: everything is surfaced
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindRepo), opts)
	t.Cleanup(cancelSub)

	var first backend.Update
	select {
	case first = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first update")
	}
	assert.Equal(t, string(backend.EventFirstPoll), string(first.Event.Type))

	// A new PR surfaces as repo-new-pr, and every update carries the resume
	// cursor (the latest createdAt in the response) so the client can persist
	// its position — the daemon-side equivalent of runRepo's AdvanceCursor.
	resp = repoFixture(1, 2)
	require.NoError(t, h.Refresh(monitor.IdentityOf(targetOf(backend.KindRepo))))
	var second backend.Update
	select {
	case second = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the new-PR update")
	}
	assert.Equal(t, string(backend.EventRepoNewPR), string(second.Event.Type))
	assert.Equal(t, "2026-08-21T00:00:00Z", second.Cursor)
}

func TestHub_SubscribeRepoClipsBySince(t *testing.T) {
	// A subscriber resuming from a cursor must not see items created at or
	// before it — the daemon-side equivalent of runRepo's filterRepoResponse.
	resp := repoFixture(1)
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return resp, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	opts := testHubOpts()
	opts.Since = "2026-08-21T00:00:00Z" // item 1 was created exactly at the cursor
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindRepo), opts)
	t.Cleanup(cancelSub)

	// First-poll still fires (it is a baseline statement, not a change), but
	// no repo-new-pr may accompany it: the only item predates the cursor.
	got := collect(ch, 100*time.Millisecond)
	assert.Contains(t, got, "first-poll")
	assert.NotContains(t, got, "repo-new-pr")
}

func TestHub_SubscribeIssueRestoresBaseline(t *testing.T) {
	// A subscriber resuming from a stored snapshot diffs its first fetch
	// against that baseline, so what changed while offline is delivered
	// without replaying the backlog (issue #32).
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return issueFixture("OPEN", "c1", "c2"), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	opts := testHubOpts()
	opts.Baseline = `{"state":"OPEN","comments":[{"id":"c1"}]}`
	ch, cancelSub := h.Subscribe(ctx, targetOf(backend.KindIssue), opts)
	t.Cleanup(cancelSub)

	got := collect(ch, 100*time.Millisecond)
	assert.NotContains(t, got, "first-poll", "a resumed watch is not a first poll")
	assert.Contains(t, got, "issue-new-comment", "the comment added while offline must be delivered")
}

func TestHub_Once(t *testing.T) {
	t.Run("emits current state and closes", func(t *testing.T) {
		h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
			return issueFixture("OPEN", "c1"), nil
		}, nil, time.Hour, nil)
		t.Cleanup(h.Stop)

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		types := waitClosed(t, h.Once(ctx, targetOf(backend.KindIssue), testHubOpts()))
		assert.Equal(t, []string{"first-poll", "issue-new-comment"}, types)
	})

	t.Run("fetch error degrades and closes", func(t *testing.T) {
		h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
			return nil, errors.New("gh api failed: exit status 1")
		}, nil, time.Hour, nil)
		t.Cleanup(h.Stop)

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		ch := h.Once(ctx, targetOf(backend.KindIssue), testHubOpts())
		var degraded bool
		deadline := time.After(2 * time.Second)
	loop:
		for {
			select {
			case u, ok := <-ch:
				if !ok {
					break loop
				}
				if u.Event.Type == backend.EventDegraded {
					degraded = true
				}
			case <-deadline:
				t.Fatal("timed out waiting for the one-shot to finish")
			}
		}
		assert.True(t, degraded, "a one-shot read has no next poll: the error must be the answer")
	})

	t.Run("baseline suppresses known state", func(t *testing.T) {
		h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
			return issueFixture("OPEN", "c1"), nil
		}, nil, time.Hour, nil)
		t.Cleanup(h.Stop)

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		opts := testHubOpts()
		opts.Baseline = `{"state":"OPEN","comments":[{"id":"c1"}]}`
		types := waitClosed(t, h.Once(ctx, targetOf(backend.KindIssue), opts))
		assert.Empty(t, types, "nothing changed since the baseline: no events")
	})
}

func TestHub_OnceDoesNotStartPoller(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	waitClosed(t, h.Once(ctx, testHubTarget(), testHubOpts()))
	h.mu.Lock()
	n := len(h.pollers)
	h.mu.Unlock()
	assert.Equal(t, 0, n, "a one-shot read must not leave a poller behind")
}

func TestHub_SubscribeKeepsSeparatePollersPerKind(t *testing.T) {
	// A PR and an issue in the same repository are different identities: each
	// gets its own poller, and a fetch for one never feeds the other.
	fetches := 0
	h := New(func(ctx context.Context, id resolver.Identity, _ monitor.QueryTier) (any, error) {
		fetches++
		if id.Target == "issue" {
			return issueFixture("OPEN", "c1"), nil
		}
		return prFixture(nil), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prCh, cancelPR := h.Subscribe(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelPR)
	issueCh, cancelIssue := h.Subscribe(ctx, targetOf(backend.KindIssue), testHubOpts())
	t.Cleanup(cancelIssue)

	prGot := collect(prCh, 100*time.Millisecond)
	issueGot := collect(issueCh, 100*time.Millisecond)
	assert.Contains(t, prGot, "first-poll")
	assert.Contains(t, issueGot, "first-poll")

	h.mu.Lock()
	n := len(h.pollers)
	h.mu.Unlock()
	assert.Equal(t, 2, n, "each kind gets its own poller")
	assert.GreaterOrEqual(t, fetches, 2, "each poller fetched independently")
}
