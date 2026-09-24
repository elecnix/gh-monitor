package hub

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphqlRateLimited is the error the gh client returns when GitHub answers a
// GraphQL query with its rate-limit error.
func graphqlRateLimited() error {
	return &ghcli.GraphQLError{Errors: []ghcli.GraphQLErrorEntry{{
		Message: "API rate limit already exceeded for user ID 1.",
	}}}
}

// commentedPR is an open, CI-green PR with one general comment, so a test
// can check that a REST read (which carries no comments) neither re-reports
// nor clears it.
func commentedPR() *monitor.PullRequest {
	pr := prFixture(nil)
	pr.Comments.Nodes = []monitor.Comment{{ID: "c1", Body: "please rename"}}
	pr.Comments.Nodes[0].Author.Login = "reviewer"
	return pr
}

// mergedPR is the REST fallback's view of the same PR after it merged.
func mergedPR() *monitor.PullRequest {
	pr := prFixture(nil)
	pr.State = "MERGED"
	pr.Merged = true
	return pr
}

// updatesUntil reads updates until one satisfies match or the timeout ends,
// and returns everything read.
func updatesUntil(t *testing.T, ch <-chan backend.Update, timeout time.Duration, match func(backend.Update) bool) []backend.Update {
	t.Helper()
	var got []backend.Update
	deadline := time.After(timeout)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, u)
			if match(u) {
				return got
			}
		case <-deadline:
			return got
		}
	}
}

func hasType(us []backend.Update, typ backend.EventType) bool {
	for _, u := range us {
		if u.Event.Type == typ {
			return true
		}
	}
	return false
}

func noticeContaining(us []backend.Update, text string) (backend.Update, bool) {
	for _, u := range us {
		if strings.Contains(u.Event.Notice, text) {
			return u, true
		}
	}
	return backend.Update{}, false
}

// TestPoller_RESTFallbackReportsMergeWhileGraphQLExhausted reproduces issue
// #123: GraphQL runs out after the first poll and the PR then merges. Without
// a REST fallback the watch only reports a degraded update, so --until merged
// waits for the budget reset. With it, the merge arrives on the next poll.
func TestPoller_RESTFallbackReportsMergeWhileGraphQLExhausted(t *testing.T) {
	var graphqlCalls, restCalls int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		if atomic.AddInt64(&graphqlCalls, 1) == 1 {
			return commentedPR(), nil
		}
		return nil, graphqlRateLimited()
	}, nil, time.Hour, nil, WithRESTFallback(func(ctx context.Context, id resolver.Identity, prev any) (any, error) {
		atomic.AddInt64(&restCalls, 1)
		assert.NotNil(t, prev, "the fallback gets the last payload, to reuse what REST cannot see")
		return mergedPR(), nil
	}))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	first := collectUpdates(ch, 300*time.Millisecond)
	require.True(t, hasType(first, monitor.EventFirstPoll))

	require.NoError(t, h.RefreshPR(monitor.IdentityOf(testHubTarget())))
	got := updatesUntil(t, ch, 2*time.Second, func(u backend.Update) bool { return u.Event.Type == monitor.EventMerged })
	require.True(t, hasType(got, monitor.EventMerged), "the merge must be reported from REST while GraphQL is exhausted")
	// Notices and snapshots use separate channels, so the notice can arrive
	// just after the event.
	got = append(got, collectUpdates(ch, 300*time.Millisecond)...)
	assert.EqualValues(t, 1, atomic.LoadInt64(&restCalls))

	notice, ok := noticeContaining(got, "reading PR state over REST because GraphQL is exhausted")
	require.True(t, ok, "the first update in REST mode must say which mode is active")
	assert.Equal(t, monitor.EventDegraded, notice.Event.Type)
	assert.ElementsMatch(t, []string{"annotations", "reviews", "review threads", "comments"}, notice.Event.DegradedSurfaces,
		"the notice lists what REST cannot read")
	assert.False(t, hasType(got, monitor.EventNewGeneralComments), "a REST read must not re-report known comments")
}

// TestPoller_RESTModeAnnouncesEachSwitch covers the mode notices: one when
// the poller starts reading over REST, none while it stays there, and one when
// GraphQL answers again. Comments known before REST mode stay known.
func TestPoller_RESTModeAnnouncesEachSwitch(t *testing.T) {
	var graphqlCalls int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		if atomic.AddInt64(&graphqlCalls, 1) == 1 {
			return commentedPR(), nil
		}
		return nil, graphqlRateLimited()
	}, nil, time.Hour, nil, WithRESTFallback(func(ctx context.Context, _ resolver.Identity, _ any) (any, error) {
		pr := prFixture(nil)
		pr.Commits.Nodes[0] = mkCommit("bbbbbbb", nil)
		return pr, nil
	}))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	collectUpdates(ch, 300*time.Millisecond)

	id := monitor.IdentityOf(testHubTarget())
	require.NoError(t, h.RefreshPR(id))
	got := collectUpdates(ch, 300*time.Millisecond)
	assert.True(t, hasType(got, monitor.EventNewCommit), "a new head read over REST is reported")
	_, ok := noticeContaining(got, "reading PR state over REST")
	require.True(t, ok)
	assert.False(t, hasType(got, monitor.EventNewGeneralComments))

	require.NoError(t, h.RefreshPR(id))
	got = collectUpdates(ch, 300*time.Millisecond)
	_, again := noticeContaining(got, "reading PR state over REST")
	assert.False(t, again, "staying in REST mode must not repeat the notice")

	swapFetcher(h, func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		pr := commentedPR()
		pr.Commits.Nodes[0] = mkCommit("bbbbbbb", nil)
		return pr, nil
	})
	require.NoError(t, h.RefreshPR(id))
	got = collectUpdates(ch, 300*time.Millisecond)
	_, ok = noticeContaining(got, "reading PR state over GraphQL again")
	require.True(t, ok, "switching back must say so")
	assert.False(t, hasType(got, monitor.EventNewGeneralComments), "comments known before REST mode are not new")
}

// TestPoller_SkipsGraphQLWhileHeadersSayExhausted: when the rate-limit headers
// of the last GraphQL response say the budget is spent until a reset time, the
// poller reads over REST without spending a failing GraphQL call, and the
// notice gives the reset time.
func TestPoller_SkipsGraphQLWhileHeadersSayExhausted(t *testing.T) {
	now := time.Now()
	reset := now.Add(25 * time.Minute)
	store := ghcli.NewRateLimitStore()
	hdr := http.Header{}
	hdr.Set("X-RateLimit-Resource", "graphql")
	hdr.Set("X-RateLimit-Limit", "5000")
	hdr.Set("X-RateLimit-Remaining", "0")
	hdr.Set("X-RateLimit-Used", "5000")
	hdr.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	store.Observe("github.com", hdr, now)

	// /rate_limit disagrees and reports a full budget, as in the issue.
	budget := monitor.NewBudgetGuard(&monitor.Service{API: &rateLimitAPIStub{remaining: 5000, limit: 5000}}, 60*time.Second)
	budget.UseObserved(store, "github.com")

	var graphqlCalls int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&graphqlCalls, 1)
		return nil, graphqlRateLimited()
	}, nil, time.Hour, budget, WithRESTFallback(func(ctx context.Context, _ resolver.Identity, _ any) (any, error) {
		return mergedPR(), nil
	}))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	got := updatesUntil(t, ch, 2*time.Second, func(u backend.Update) bool { return u.Event.Type == monitor.EventFirstPoll })
	require.True(t, hasType(got, monitor.EventFirstPoll))
	got = append(got, collectUpdates(ch, 300*time.Millisecond)...)
	assert.Zero(t, atomic.LoadInt64(&graphqlCalls), "an exhausted budget must not be spent on a failing GraphQL call")

	notice, ok := noticeContaining(got, "reading PR state over REST because GraphQL is exhausted until "+reset.Local().Format("15:04"))
	require.True(t, ok, "the notice gives the reset time from the headers")
	assert.Equal(t, reset.UTC().Format(time.RFC3339), notice.Event.DegradedResetAt)
}

// TestPoller_NoFallbackForNonRateLimitErrors: REST mode is for an exhausted
// GraphQL budget. Other failures keep the degraded path.
func TestPoller_NoFallbackForNonRateLimitErrors(t *testing.T) {
	var restCalls int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, &ghcli.APIError{StatusCode: 502, Message: "Bad Gateway"}
	}, nil, time.Hour, nil, WithRESTFallback(func(ctx context.Context, _ resolver.Identity, _ any) (any, error) {
		atomic.AddInt64(&restCalls, 1)
		return mergedPR(), nil
	}))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)

	waitDegraded(t, ch, "a non-rate-limit failure still reports degraded")
	assert.Zero(t, atomic.LoadInt64(&restCalls))
}

func collectUpdates(ch <-chan backend.Update, timeout time.Duration) []backend.Update {
	var out []backend.Update
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, u)
		case <-time.After(timeout):
			return out
		}
	}
}

// TestOnce_RESTFallbackAnswersWhileGraphQLExhausted: a --once read of a PR
// also reads over REST when GraphQL is rate limited, and says so.
func TestOnce_RESTFallbackAnswersWhileGraphQLExhausted(t *testing.T) {
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return nil, graphqlRateLimited()
	}, nil, time.Hour, nil, WithRESTFallback(func(ctx context.Context, _ resolver.Identity, prev any) (any, error) {
		assert.Nil(t, prev, "a one-shot read has no previous payload")
		return mergedPR(), nil
	}))
	t.Cleanup(h.Stop)

	opts := testHubOpts()
	opts.Once = true
	got := collectUpdates(h.Once(context.Background(), testHubTarget(), opts), time.Second)
	assert.True(t, hasType(got, monitor.EventMerged), "the one-shot read reports the merge from REST")
	_, ok := noticeContaining(got, "reading PR state over REST because GraphQL is exhausted")
	assert.True(t, ok)
}

// TestPoller_RESTModeDeclaresItsWindow: REST mode is a blind window for
// comments, review threads, reviews and annotations, so both mode notices
// carry it like the #99 recovery notice. degraded_from is the last successful
// GraphQL read, not the moment the poller found GraphQL refused. The exit
// notice adds degraded_to.
func TestPoller_RESTModeDeclaresItsWindow(t *testing.T) {
	var graphqlCalls int64
	var firstGraphQL time.Time
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		if atomic.AddInt64(&graphqlCalls, 1) == 1 {
			firstGraphQL = time.Now()
			return commentedPR(), nil
		}
		return nil, graphqlRateLimited()
	}, nil, time.Hour, nil, WithRESTFallback(func(ctx context.Context, _ resolver.Identity, _ any) (any, error) {
		return prFixture(nil), nil
	}))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), testHubOpts())
	t.Cleanup(cancelSub)
	collectUpdates(ch, 300*time.Millisecond)

	// Let the discovery moment differ from the last GraphQL read.
	time.Sleep(1100 * time.Millisecond)
	id := monitor.IdentityOf(testHubTarget())
	require.NoError(t, h.RefreshPR(id))
	entry, ok := noticeContaining(collectUpdates(ch, 300*time.Millisecond), "reading PR state over REST")
	require.True(t, ok)
	require.NotEmpty(t, entry.Event.DegradedFrom, "the entry notice gives the window start")
	from, err := time.Parse(time.RFC3339, entry.Event.DegradedFrom)
	require.NoError(t, err)
	assert.WithinDuration(t, firstGraphQL, from, time.Second, "the window starts at the last GraphQL read")
	assert.Contains(t, entry.Event.Notice, "ends the watch without", "a terminal event read over REST has no catch-up read")

	swapFetcher(h, func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return commentedPR(), nil
	})
	require.NoError(t, h.RefreshPR(id))
	exit, ok := noticeContaining(collectUpdates(ch, 300*time.Millisecond), "over GraphQL again")
	require.True(t, ok)
	assert.Equal(t, entry.Event.DegradedFrom, exit.Event.DegradedFrom)
	require.NotEmpty(t, exit.Event.DegradedTo, "the exit notice closes the window")
	assert.Contains(t, exit.Event.Notice, exit.Event.DegradedFrom)
	assert.Contains(t, exit.Event.Notice, "backfill")
}

// TestPoller_SkipsGraphQLForEnterpriseHostWhileHeadersSayExhausted: a GitHub
// Enterprise client records header readings under its own host, so the
// poller must ask the guard about the target's host, not github.com.
func TestPoller_SkipsGraphQLForEnterpriseHostWhileHeadersSayExhausted(t *testing.T) {
	now := time.Now()
	store := ghcli.NewRateLimitStore()
	hdr := http.Header{}
	hdr.Set("X-RateLimit-Resource", "graphql")
	hdr.Set("X-RateLimit-Limit", "5000")
	hdr.Set("X-RateLimit-Remaining", "0")
	hdr.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(25*time.Minute).Unix(), 10))
	store.Observe("ghe.example.com", hdr, now)

	budget := monitor.NewBudgetGuard(&monitor.Service{API: &rateLimitAPIStub{remaining: 5000, limit: 5000}}, 60*time.Second)
	budget.UseObserved(store, "github.com")

	var graphqlCalls int64
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		atomic.AddInt64(&graphqlCalls, 1)
		return nil, graphqlRateLimited()
	}, nil, time.Hour, budget, WithRESTFallback(func(ctx context.Context, _ resolver.Identity, _ any) (any, error) {
		return prFixture(nil), nil
	}))
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := testHubTarget()
	target.Host = "ghe.example.com"
	ch, cancelSub := h.SubscribePR(ctx, target, testHubOpts())
	t.Cleanup(cancelSub)

	got := collectUpdates(ch, 500*time.Millisecond)
	require.True(t, hasType(got, monitor.EventFirstPoll))
	assert.Zero(t, atomic.LoadInt64(&graphqlCalls), "the enterprise host's readings say GraphQL is spent")
}
