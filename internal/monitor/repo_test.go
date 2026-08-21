package monitor

import (
	"errors"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Repo API / Snapshot tests
// ---------------------------------------------------------------------------

func TestFetchRepo(t *testing.T) {
	t.Run("sends query and unmarshals", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Contains(t, query, "MonitorRepo")
			assert.Equal(t, "octocat", variables["owner"])
			assert.Equal(t, "hello", variables["repo"])
			assert.Equal(t, 25, variables["first"])
			return assign(result, RepoQueryResponse{
				Repository: struct {
					PullRequests RepoPRNodes    `json:"pullRequests"`
					Issues       RepoIssueNodes `json:"issues"`
				}{
					PullRequests: RepoPRNodes{Nodes: []RepoPR{
						{Number: 1, Title: "fix bug", State: "OPEN", URL: "https://github.com/octocat/hello/pull/1"},
					}},
					Issues: RepoIssueNodes{Nodes: []RepoIssue{
						{Number: 42, Title: "crash on start", State: "OPEN", URL: "https://github.com/octocat/hello/issues/42"},
					}},
				},
			})
		}}
		svc := &Service{API: api}
		got, err := svc.FetchRepo("octocat", "hello")
		require.NoError(t, err)
		require.Len(t, got.Repository.PullRequests.Nodes, 1)
		assert.Equal(t, 1, got.Repository.PullRequests.Nodes[0].Number)
		assert.Equal(t, "fix bug", got.Repository.PullRequests.Nodes[0].Title)
		require.Len(t, got.Repository.Issues.Nodes, 1)
		assert.Equal(t, 42, got.Repository.Issues.Nodes[0].Number)
	})

	t.Run("propagates API error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return errors.New("boom")
		}}
		svc := &Service{API: api}
		_, err := svc.FetchRepo("o", "r")
		require.Error(t, err)
	})
}

func TestSnapshotRepo(t *testing.T) {
	resp := &RepoQueryResponse{
		Repository: struct {
			PullRequests RepoPRNodes    `json:"pullRequests"`
			Issues       RepoIssueNodes `json:"issues"`
		}{
			PullRequests: RepoPRNodes{Nodes: []RepoPR{
				{Number: 1, Title: "feat: add x", State: "OPEN", URL: "https://gh/o/r/pull/1",
					CreatedAt: "2024-01-01T00:00:00Z",
					Author: struct {
						Login string `json:"login"`
					}{Login: "alice"}},
				{Number: 2, Title: "fix: bug", State: "OPEN", URL: "https://gh/o/r/pull/2",
					CreatedAt: "2024-01-02T00:00:00Z",
					Author: struct {
						Login string `json:"login"`
					}{Login: "bob"}},
			}},
			Issues: RepoIssueNodes{Nodes: []RepoIssue{
				{Number: 10, Title: "crash", State: "OPEN", URL: "https://gh/o/r/issues/10",
					CreatedAt: "2024-01-01T00:00:00Z",
					Author: struct {
						Login string `json:"login"`
					}{Login: "carol"}},
			}},
		},
	}
	s := SnapshotRepo(resp)
	assert.Len(t, s.PRs, 2)
	assert.Equal(t, 1, s.PRs[0].Number)
	assert.Equal(t, "alice", s.PRs[0].Author)
	assert.Len(t, s.Issues, 1)
	assert.Equal(t, 10, s.Issues[0].Number)
	assert.Equal(t, "carol", s.Issues[0].Author)
}

func TestSnapshotRepo_Empty(t *testing.T) {
	resp := &RepoQueryResponse{}
	s := SnapshotRepo(resp)
	assert.Empty(t, s.PRs)
	assert.Empty(t, s.Issues)
}

// ---------------------------------------------------------------------------
// DiffRepo tests
// ---------------------------------------------------------------------------

func TestDiffRepo_NewPRs(t *testing.T) {
	prev := &RepoStatus{
		PRs:    []RepoItemSummary{{Number: 1, Title: "old", Author: "alice"}},
		Issues: nil,
	}
	curr := &RepoStatus{
		PRs: []RepoItemSummary{
			{Number: 1, Title: "old", Author: "alice"},
			{Number: 2, Title: "new pr", Author: "bob", URL: "https://gh/o/r/pull/2"},
		},
		Issues: nil,
	}
	events := DiffRepo(prev, curr)
	require.Len(t, events, 1)
	assert.Equal(t, EventRepoNewPR, events[0].Type)
	require.Len(t, events[0].RepoItems, 1)
	assert.Equal(t, 2, events[0].RepoItems[0].Number)
}

func TestDiffRepo_NewIssues(t *testing.T) {
	prev := &RepoStatus{
		PRs:    nil,
		Issues: []RepoItemSummary{{Number: 10, Title: "old issue", Author: "carol"}},
	}
	curr := &RepoStatus{
		PRs: nil,
		Issues: []RepoItemSummary{
			{Number: 10, Title: "old issue", Author: "carol"},
			{Number: 11, Title: "new issue", Author: "dave", URL: "https://gh/o/r/issues/11"},
		},
	}
	events := DiffRepo(prev, curr)
	require.Len(t, events, 1)
	assert.Equal(t, EventRepoNewIssue, events[0].Type)
	require.Len(t, events[0].RepoItems, 1)
	assert.Equal(t, 11, events[0].RepoItems[0].Number)
}

func TestDiffRepo_MultipleNew(t *testing.T) {
	prev := &RepoStatus{}
	curr := &RepoStatus{
		PRs: []RepoItemSummary{
			{Number: 1, Title: "pr1", Author: "a", URL: "url1"},
			{Number: 2, Title: "pr2", Author: "b", URL: "url2"},
		},
		Issues: []RepoItemSummary{
			{Number: 10, Title: "issue1", Author: "c", URL: "url3"},
		},
	}
	events := DiffRepo(prev, curr)
	assert.Len(t, events, 3) // 2 PRs + 1 issue
	prCount, issueCount := 0, 0
	for _, ev := range events {
		switch ev.Type {
		case EventRepoNewPR:
			prCount++
		case EventRepoNewIssue:
			issueCount++
		}
	}
	assert.Equal(t, 2, prCount)
	assert.Equal(t, 1, issueCount)
}

func TestDiffRepo_NoChange(t *testing.T) {
	prev := &RepoStatus{
		PRs:    []RepoItemSummary{{Number: 1, Title: "same", Author: "a"}},
		Issues: []RepoItemSummary{{Number: 10, Title: "same", Author: "b"}},
	}
	curr := &RepoStatus{
		PRs:    []RepoItemSummary{{Number: 1, Title: "same", Author: "a"}},
		Issues: []RepoItemSummary{{Number: 10, Title: "same", Author: "b"}},
	}
	events := DiffRepo(prev, curr)
	assert.Empty(t, events)
}

func TestDiffRepo_NilPrev(t *testing.T) {
	curr := &RepoStatus{
		PRs:    []RepoItemSummary{{Number: 1}},
		Issues: []RepoItemSummary{{Number: 10}},
	}
	events := DiffRepo(nil, curr)
	assert.Empty(t, events)
}

// ---------------------------------------------------------------------------
// Run / Once repo tests
// ---------------------------------------------------------------------------

func repoRunOptions() RunOptions {
	return RunOptions{
		Identity: resolver.Identity{Owner: "o", Repo: "r", Target: "repo", Host: "github.com"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 60,
		Now:      func() time.Time { return time.Unix(0, 0).UTC() },
	}
}

func TestRenderNotificationRepo(t *testing.T) {
	status := &RepoStatus{
		PRs:    []RepoItemSummary{{Number: 1, Title: "feat: x", Author: "alice", URL: "https://gh/o/r/pull/1"}},
		Issues: []RepoItemSummary{{Number: 42, Title: "bug", Author: "bob", URL: "https://gh/o/r/issues/42"}},
	}
	opts := repoRunOptions()

	// Test PR notification
	ev := Event{Type: EventRepoNewPR, RepoItems: []RepoItemSummary{status.PRs[0]}}
	n := renderNotificationRepo(opts, status, string(EventRepoNewPR), ev)
	assert.Equal(t, string(EventRepoNewPR), n.Type)
	assert.Contains(t, n.Message, "#1")
	assert.Contains(t, n.Message, "feat: x")

	// Test issue notification
	ev2 := Event{Type: EventRepoNewIssue, RepoItems: []RepoItemSummary{status.Issues[0]}}
	n2 := renderNotificationRepo(opts, status, string(EventRepoNewIssue), ev2)
	assert.Equal(t, string(EventRepoNewIssue), n2.Type)
	assert.Contains(t, n2.Message, "#42")
	assert.Contains(t, n2.Message, "bug")

	// Test first-poll notification
	n3 := renderNotificationRepo(opts, status, firstPollType, Event{})
	assert.Equal(t, firstPollType, n3.Type)
	assert.Contains(t, n3.Message, "o/r")
}

// ---------------------------------------------------------------------------
// Helpers for repo tests
// ---------------------------------------------------------------------------
