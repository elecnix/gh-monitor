package monitor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/ghcli"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restPRAPI serves the REST endpoints FetchPRViaREST reads. The payloads use
// the field names and lower-case enum values GitHub's REST API returns.
func restPRAPI(t *testing.T, pull map[string]any, served map[string]int) *fakeAPI {
	t.Helper()
	return &fakeAPI{
		graphqlFunc: func(string, map[string]interface{}, interface{}) error {
			t.Fatal("the REST read must not call GraphQL")
			return nil
		},
		restFunc: func(method, path string, _ map[string]string, _ interface{}, result interface{}) error {
			if method != "GET" {
				return fmt.Errorf("unexpected method %s", method)
			}
			key := path
			if i := strings.IndexByte(key, '?'); i >= 0 {
				key = key[:i]
			}
			served[key]++
			switch key {
			case "repos/o/r/pulls/7":
				return assign(result, pull)
			case "repos/o/r/commits/bbbbbbb/check-runs":
				return assign(result, map[string]any{
					"total_count": 2,
					"check_runs": []map[string]any{
						{
							"name": "build", "status": "completed", "conclusion": "failure",
							"started_at": "2026-09-24T10:00:00Z", "completed_at": "2026-09-24T10:05:00Z",
							"details_url": "https://example.test/build", "html_url": "https://github.com/o/r/runs/1",
							"app":         map[string]any{"name": "GitHub Actions", "slug": "github-actions"},
							"check_suite": map[string]any{"id": 11},
						},
						{
							"name": "lint", "status": "completed", "conclusion": "success",
							"started_at": "2026-09-24T10:00:00Z", "completed_at": "2026-09-24T10:01:00Z",
							"app":         map[string]any{"name": "GitHub Actions", "slug": "github-actions"},
							"check_suite": map[string]any{"id": 11},
						},
						{
							"name": "e2e", "status": "in_progress", "conclusion": nil,
							"app":         map[string]any{"name": "Other CI", "slug": "other-ci"},
							"check_suite": map[string]any{"id": 12},
						},
					},
				})
			case "repos/o/r/commits/bbbbbbb/check-suites":
				return assign(result, map[string]any{
					"total_count": 2,
					"check_suites": []map[string]any{
						{"id": 11, "status": "completed", "conclusion": "failure",
							"app": map[string]any{"name": "GitHub Actions", "slug": "github-actions"}},
						{"id": 12, "status": "in_progress", "conclusion": nil,
							"app": map[string]any{"name": "Other CI", "slug": "other-ci"}},
					},
				})
			case "repos/o/r/commits/bbbbbbb/status":
				return assign(result, map[string]any{
					"state": "failure",
					"statuses": []map[string]any{
						{"state": "failure", "context": "ext-ci", "description": "broke", "target_url": "https://ci.example.test"},
					},
				})
			case "repos/o/r/commits/bbbbbbb":
				return assign(result, map[string]any{
					"sha": "bbbbbbb",
					"commit": map[string]any{
						"message": "Fix the parser\n\nCo-authored-by: Ada <ada@example.test>",
						"author":  map[string]any{"name": "Nick"},
					},
					"author": map[string]any{"login": "nick"},
				})
			}
			return fmt.Errorf("unexpected REST path %s", path)
		},
	}
}

func TestFetchPRViaREST_ReportsMergeHeadAndChecks(t *testing.T) {
	served := map[string]int{}
	api := restPRAPI(t, map[string]any{
		"state": "closed", "merged": true, "merged_at": "2026-09-24T10:10:00Z",
		"merge_commit_sha": "mmmmmmm", "mergeable": nil, "mergeable_state": "unknown",
		"head": map[string]any{"sha": "bbbbbbb"},
	}, served)
	svc := &Service{API: api}

	pr, err := svc.FetchPRViaREST("o", "r", 7, nil)
	require.NoError(t, err)
	assert.Equal(t, "MERGED", pr.State, "REST reports a merged PR as closed+merged; GraphQL calls it MERGED")
	assert.True(t, pr.Merged)
	assert.Equal(t, "UNKNOWN", pr.Mergeable)

	st := Snapshot(pr, SnapshotOptions{Tier: TierStatus})
	assert.True(t, st.Merged)
	assert.ElementsMatch(t, []string{"build", "ext-ci"}, st.FailingChecks)
	assert.Equal(t, []string{"Other CI"}, st.PendingChecks, "a pending suite is reported by its app name, as over GraphQL")
	assert.Contains(t, st.SuccessfulChecks, "lint")
	assert.Equal(t, "bbbbbbb", st.LastCommit.Oid)
	assert.Equal(t, "Fix the parser", st.LastCommit.MessageHeadline)
	assert.Equal(t, "nick", st.LastCommit.Author)
	assert.Equal(t, []string{"Ada"}, st.LastCommit.Coauthors)
	assert.Equal(t, 1, served["repos/o/r/commits/bbbbbbb"], "a new head is read once for its message")
}

func TestFetchPRViaREST_ReusesKnownHeadCommit(t *testing.T) {
	served := map[string]int{}
	api := restPRAPI(t, map[string]any{
		"state": "open", "merged": false, "mergeable": false, "mergeable_state": "dirty",
		"head": map[string]any{"sha": "bbbbbbb"},
	}, served)
	svc := &Service{API: api}

	prev := &PullRequest{State: "OPEN", Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
		Oid: "bbbbbbb", MessageHeadline: "known headline", Message: "known headline",
	}}}}}
	pr, err := svc.FetchPRViaREST("o", "r", 7, prev)
	require.NoError(t, err)
	assert.Equal(t, "OPEN", pr.State)
	assert.Equal(t, "CONFLICTING", pr.Mergeable)
	assert.Equal(t, "DIRTY", pr.MergeState)
	assert.True(t, Snapshot(pr, SnapshotOptions{}).Conflict)
	assert.Equal(t, "known headline", pr.Commits.Nodes[0].Commit.MessageHeadline)
	assert.Zero(t, served["repos/o/r/commits/bbbbbbb"], "an unchanged head needs no commit read")
}

func TestIsRateLimitError(t *testing.T) {
	assert.True(t, IsRateLimitError(&ghcli.GraphQLError{Errors: []ghcli.GraphQLErrorEntry{{Message: "API rate limit already exceeded for user ID 1."}}}))
	assert.True(t, IsRateLimitError(&ghcli.APIError{StatusCode: 403, Message: "API rate limit exceeded for user ID 1."}))
	assert.True(t, IsRateLimitError(&ghcli.APIError{StatusCode: 429, Message: "Too Many Requests"}))
	assert.False(t, IsRateLimitError(&ghcli.APIError{StatusCode: 404, Message: "Not Found"}))
	assert.False(t, IsRateLimitError(fmt.Errorf("exit status 1")))
	assert.False(t, IsRateLimitError(nil))
}

// checksAPI serves an open PR whose head is ccccccc, with the given check
// suites and check runs. runsTotal is the total_count the check-runs endpoint
// reports; runs are served 100 per page from the runs slice.
func checksAPI(t *testing.T, suites []map[string]any, runs []map[string]any, runsTotal int) *fakeAPI {
	t.Helper()
	return &fakeAPI{restFunc: func(method, path string, _ map[string]string, _ interface{}, result interface{}) error {
		base, query, _ := strings.Cut(path, "?")
		page := 1
		for _, kv := range strings.Split(query, "&") {
			if v, ok := strings.CutPrefix(kv, "page="); ok {
				_, _ = fmt.Sscanf(v, "%d", &page)
			}
		}
		switch base {
		case "repos/o/r/pulls/7":
			return assign(result, map[string]any{"state": "open", "merged": false, "mergeable": true,
				"mergeable_state": "clean", "head": map[string]any{"sha": "ccccccc"}})
		case "repos/o/r/commits/ccccccc/check-suites":
			if page > 1 {
				return assign(result, map[string]any{"total_count": len(suites), "check_suites": []any{}})
			}
			return assign(result, map[string]any{"total_count": len(suites), "check_suites": suites})
		case "repos/o/r/commits/ccccccc/check-runs":
			lo, hi := (page-1)*100, page*100
			if lo > len(runs) {
				lo = len(runs)
			}
			if hi > len(runs) {
				hi = len(runs)
			}
			return assign(result, map[string]any{"total_count": runsTotal, "check_runs": runs[lo:hi]})
		case "repos/o/r/commits/ccccccc/status":
			return assign(result, map[string]any{"state": "success", "statuses": []any{}})
		}
		return fmt.Errorf("unexpected REST path %s", path)
	}}
}

func knownHead() *PullRequest {
	return &PullRequest{State: "OPEN", Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{Oid: "ccccccc", MessageHeadline: "h"}}}}}
}

func suite(id int64, app, slug, status string, conclusion any) map[string]any {
	return map[string]any{"id": id, "status": status, "conclusion": conclusion,
		"app": map[string]any{"name": app, "slug": slug}}
}

func actionsRun(name string, suiteID int64) map[string]any {
	return map[string]any{"name": name, "status": "completed", "conclusion": "success",
		"app":         map[string]any{"name": "GitHub Actions", "slug": "github-actions"},
		"check_suite": map[string]any{"id": suiteID}}
}

// TestFetchPRViaREST_RunlessSuitesKeepCIFromReadingGreen reproduces the
// review of #124, measured on the PR's own head: two third-party suites are
// queued and have no check runs, beside green GitHub Actions suites. The
// check-runs endpoint cannot list them, so a REST read built only from runs
// reported CI green and Diff emitted a false ci-all-green.
func TestFetchPRViaREST_RunlessSuitesKeepCIFromReadingGreen(t *testing.T) {
	api := checksAPI(t, []map[string]any{
		suite(1, "sonatype-lift", "sonatype-lift", "queued", nil),
		suite(2, "Claude", "claude", "queued", nil),
		suite(3, "GitHub Actions", "github-actions", "completed", "success"),
		suite(4, "GitHub Actions", "github-actions", "completed", "success"),
	}, []map[string]any{actionsRun("test", 3), actionsRun("lint", 3), actionsRun("build", 4)}, 3)

	pr, err := (&Service{API: api}).FetchPRViaREST("o", "r", 7, knownHead())
	require.NoError(t, err)
	curr := Snapshot(pr, SnapshotOptions{Tier: TierStatus})
	assert.ElementsMatch(t, []string{"sonatype-lift", "Claude"}, curr.PendingChecks)

	prev := &PRStatus{State: "OPEN", PendingChecks: []string{"sonatype-lift", "Claude"}, LastCommit: curr.LastCommit}
	for _, ev := range Diff(prev, curr) {
		assert.NotEqual(t, EventCIAllGreen, ev.Type, "queued suites without runs are not green")
	}
}

// TestFetchPRViaREST_FailingRunlessSuiteFails: a non-container suite that
// concluded failure without check runs is a failing check, as over GraphQL.
func TestFetchPRViaREST_FailingRunlessSuiteFails(t *testing.T) {
	api := checksAPI(t, []map[string]any{
		suite(1, "Lint App", "lint-app", "completed", "failure"),
		suite(3, "GitHub Actions", "github-actions", "completed", "success"),
	}, []map[string]any{actionsRun("test", 3)}, 1)

	pr, err := (&Service{API: api}).FetchPRViaREST("o", "r", 7, knownHead())
	require.NoError(t, err)
	assert.Equal(t, []string{"Lint App"}, Snapshot(pr, SnapshotOptions{}).FailingChecks)
}

// TestFetchPRViaREST_TruncatedRunsMarkTheSnapshot: when the read stops at its
// page cap with runs still unread, the snapshot says it is truncated, so
// ciAllGreen refuses to report green on it.
func TestFetchPRViaREST_TruncatedRunsMarkTheSnapshot(t *testing.T) {
	runs := make([]map[string]any, 0, 600)
	for i := 0; i < 600; i++ {
		runs = append(runs, actionsRun(fmt.Sprintf("job-%d", i), 3))
	}
	api := checksAPI(t, []map[string]any{suite(3, "GitHub Actions", "github-actions", "completed", "success")}, runs, 600)

	pr, err := (&Service{API: api}).FetchPRViaREST("o", "r", 7, knownHead())
	require.NoError(t, err)
	st := Snapshot(pr, SnapshotOptions{})
	assert.True(t, st.TruncatedSuites, "500 of 600 runs read is an incomplete payload")
	assert.False(t, ciAllGreen(st))
}
