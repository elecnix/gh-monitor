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
