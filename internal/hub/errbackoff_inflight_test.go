package hub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #231: a failed fetch must not park a target whose checks are still in
// flight. These tests drive a poller's fetches by hand and read the wait it
// schedules after each one, so no timer or wall clock is involved.

const inFlightBase = 30 * time.Second

// jitterCeiling is the longest wait monitor.Jittered returns for base.
func jitterCeiling(base time.Duration) time.Duration { return base + base/5 }

// prWithHeadChecks builds an open pull request whose head commit carries the
// given suites and old-style status contexts.
func prWithHeadChecks(suites []monitor.CheckSuite, contexts ...monitor.StatusContext) *monitor.PullRequest {
	details := monitor.CommitDetails{
		Oid:             "aaaaaaa",
		MessageHeadline: "headline",
		CheckSuites:     monitor.SuiteNodes{Nodes: suites, TotalCount: len(suites)},
	}
	if len(contexts) > 0 {
		details.Status = &monitor.CommitStatus{Contexts: contexts}
	}
	return &monitor.PullRequest{
		State:   "OPEN",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: details}}},
	}
}

// actionsSuite builds a GitHub Actions suite around one run. A run that has
// not finished has a null conclusion, and so does its suite.
func actionsSuite(status, conclusion string) monitor.CheckSuite {
	run := monitor.CheckRun{Name: "build", Status: status, Conclusion: conclusion}
	return monitor.CheckSuite{
		Status:     status,
		Conclusion: conclusion,
		App:        monitor.AppInfo{Name: "GitHub Actions", Slug: "github-actions"},
		CheckRuns:  monitor.RunNodes{Nodes: []monitor.CheckRun{run}, TotalCount: 1},
	}
}

func pendingRunPR() *monitor.PullRequest {
	return prWithHeadChecks([]monitor.CheckSuite{actionsSuite("IN_PROGRESS", "")})
}

func pendingContextPR() *monitor.PullRequest {
	return prWithHeadChecks(
		[]monitor.CheckSuite{actionsSuite("COMPLETED", "SUCCESS")},
		monitor.StatusContext{State: "PENDING", Context: "ci/deploy"},
	)
}

func settledPR() *monitor.PullRequest {
	return prWithHeadChecks(
		[]monitor.CheckSuite{actionsSuite("COMPLETED", "SUCCESS")},
		monitor.StatusContext{State: "SUCCESS", Context: "ci/deploy"},
	)
}

// scriptedFetch answers a poller's fetches with whatever the test set last.
type scriptedFetch struct {
	mu      sync.Mutex
	payload any
	err     error
}

func (s *scriptedFetch) answer(payload any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payload, s.err = payload, err
}

func (s *scriptedFetch) fetch(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.payload, s.err
}

// pollerAfterFetch returns a poller of the given kind whose one successful
// fetch produced payload, with the script that answers its next fetches.
func pollerAfterFetch(t *testing.T, kind backend.Kind, payload any) (*poller, *scriptedFetch) {
	t.Helper()
	script := &scriptedFetch{payload: payload}
	h := New(script.fetch, nil, inFlightBase, nil)
	t.Cleanup(h.Stop)
	p := newPoller(h, monitor.IdentityOf(targetOf(kind)), inFlightBase)
	p.fetchOnce()
	p.mu.Lock()
	latest := p.latest
	p.mu.Unlock()
	require.NotNil(t, latest, "the first fetch must leave a snapshot")
	return p, script
}

// failFetches makes the poller's next n fetches fail the way a GraphQL 504
// does, and returns the wait scheduled after each one.
func failFetches(p *poller, script *scriptedFetch, n int) []time.Duration {
	script.answer(nil, errors.New("gh api graphql: HTTP 504: gateway timeout"))
	waits := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		p.fetchOnce()
		waits = append(waits, p.nextDelay())
	}
	return waits
}

func errBackoffOf(p *poller) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errBackoff
}

func TestPoller_FailedFetchKeepsBaseCadenceWhileChecksInFlight(t *testing.T) {
	cases := []struct {
		name    string
		kind    backend.Kind
		payload any
	}{
		{"pull request with a pending check run", backend.KindPR, pendingRunPR()},
		{"pull request with a pending status context", backend.KindPR, pendingContextPR()},
		{"workflow run still in progress", backend.KindRun, runFixture("in_progress", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, script := pollerAfterFetch(t, tc.kind, tc.payload)
			// Eight failures take the error backoff from 30s to its 300s cap.
			for i, wait := range failFetches(p, script, 8) {
				assert.LessOrEqual(t, wait, jitterCeiling(inFlightBase),
					"failure %d scheduled the next fetch later than the base interval", i+1)
				assert.GreaterOrEqual(t, wait, inFlightBase-inFlightBase/5,
					"failure %d scheduled the next fetch sooner than the base interval", i+1)
			}
		})
	}
}

func TestPoller_FailedFetchKeepsBaseCadenceForRefAndCommitChecks(t *testing.T) {
	suites := monitor.SuiteNodes{Nodes: []monitor.CheckSuite{actionsSuite("QUEUED", "")}, TotalCount: 1}

	ref := refFixture("aaaaaaa", nil)
	ref.Repository.Ref.Target.CheckSuites = suites
	var commit monitor.CommitQueryResponse
	commit.Repository.Object = &monitor.CommitObject{Oid: "aaaaaaa", CheckSuites: suites}

	for kind, payload := range map[backend.Kind]any{backend.KindRef: ref, backend.KindCommit: &commit} {
		t.Run(string(kind), func(t *testing.T) {
			p, script := pollerAfterFetch(t, kind, payload)
			require.Equal(t, kind, p.key.kind)
			for i, wait := range failFetches(p, script, 8) {
				assert.LessOrEqual(t, wait, jitterCeiling(inFlightBase), "failure %d", i+1)
			}
		})
	}
}

func TestPoller_FailedFetchKeepsErrorCeilingWhenNothingIsInFlight(t *testing.T) {
	cases := []struct {
		name    string
		kind    backend.Kind
		payload any
	}{
		{"pull request with every check finished", backend.KindPR, settledPR()},
		{"pull request with no checks", backend.KindPR, prWithHeadChecks(nil)},
		{"issue", backend.KindIssue, issueFixture("OPEN")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, script := pollerAfterFetch(t, tc.kind, tc.payload)
			waits := failFetches(p, script, 8)
			assert.Equal(t, 300*time.Second, errBackoffOf(p), "the error backoff still doubles to its cap")
			assert.GreaterOrEqual(t, waits[len(waits)-1], 240*time.Second,
				"a target with nothing in flight waits out the 300s error ceiling")
		})
	}
}

func TestPoller_SuccessResetsErrorBackoffWhateverTheChecksSay(t *testing.T) {
	for name, payload := range map[string]*monitor.PullRequest{
		"checks in flight": pendingRunPR(),
		"checks finished":  settledPR(),
	} {
		t.Run(name, func(t *testing.T) {
			p, script := pollerAfterFetch(t, backend.KindPR, payload)
			failFetches(p, script, 4)
			require.Equal(t, 240*time.Second, errBackoffOf(p))

			script.answer(settledPR(), nil)
			p.fetchOnce()
			assert.Zero(t, errBackoffOf(p), "a successful fetch resets the error backoff")
			assert.LessOrEqual(t, p.nextDelay(), jitterCeiling(inFlightBase))
		})
	}
}

// TestPoller_BudgetPaceOutlastsInFlightRule pins the order of the two rules.
// The in-flight rule removes the error backoff. It does not remove the budget
// pace from #213, which is the wait until GitHub will answer at all, so a
// stretched pace is still the wait that is scheduled.
func TestPoller_BudgetPaceOutlastsInFlightRule(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := ghcli.NewRateLimitStore()
	store.Observe("", ghcli.GraphQLHeaders(900, 5000, now.Add(time.Hour)), now)
	h := paceHub(t, store, &now)
	p := addPollers(h, 3, 26)[0]

	pace, stretched := h.paceInterval()
	require.True(t, stretched, "pace %v", pace)
	require.Greater(t, pace, jitterCeiling(h.interval))

	p.mu.Lock()
	p.latest = pendingRunPR()
	p.errBackoff = 300 * time.Second
	p.mu.Unlock()

	assert.Equal(t, pace, p.nextDelay(),
		"the budget pace is the wait even while checks are in flight and fetches fail")
}
