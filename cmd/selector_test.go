package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonitorShorthandSelector covers the OWNER/REPO#NUMBER shorthand on the
// default command: it names its own repository, so it needs no -R and no git
// context, and it resolves to the same identity as -R owner/repo NUMBER.
func TestMonitorShorthandSelector(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	var gotOwner, gotRepo string
	var gotNumber int
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorPR")
		gotOwner, _ = variables["owner"].(string)
		gotRepo, _ = variables["repo"].(string)
		gotNumber, _ = variables["number"].(int)
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"octo/demo#7", "--once"})
	require.NoError(t, root.Execute())

	assert.Equal(t, "octo", gotOwner)
	assert.Equal(t, "demo", gotRepo)
	assert.Equal(t, 7, gotNumber)
	assert.Contains(t, stdout.String(), `"pr_label":"octo/demo#7"`)
}

// TestMonitorShorthandSelectorUsesGHHost checks the host rule the shorthand
// follows: it carries no host of its own, so it names a repository on the
// host the command is already talking to.
func TestMonitorShorthandSelectorUsesGHHost(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "ghe.example.com")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	var gotHost string
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(host string) ghcli.API {
		gotHost = host
		return fake
	}

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"octo/demo#7", "--once"})
	require.NoError(t, root.Execute())

	assert.Equal(t, "ghe.example.com", gotHost)
}

// TestMonitorShorthandSelectorMatchesRepoFlag is the equivalence the issue
// asks for: the shorthand and -R owner/repo NUMBER name the same target.
func TestMonitorShorthandSelectorMatchesRepoFlag(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	type target struct {
		owner  string
		repo   string
		number int
	}
	var seen []target
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		owner, _ := variables["owner"].(string)
		repo, _ := variables["repo"].(string)
		number, _ := variables["number"].(int)
		seen = append(seen, target{owner, repo, number})
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	run := func(args []string) {
		root := newRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		require.NoError(t, root.Execute())
	}

	run([]string{"octo/demo#7", "--once"})
	run([]string{"-R", "octo/demo", "7", "--once"})

	require.Len(t, seen, 2)
	assert.Equal(t, seen[0], seen[1], "the shorthand and -R owner/repo NUMBER must name the same target")
}

// TestMonitorShorthandSelectorWithPrFlag covers the --pr consistency rule:
// a matching --pr is accepted, a mismatching one is rejected — the same
// rule a URL selector follows.
func TestMonitorShorthandSelectorWithPrFlag(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	// A matching --pr is accepted.
	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"octo/demo#7", "--pr", "7", "--once"})
	require.NoError(t, root.Execute())

	// A mismatching --pr is rejected.
	root = newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"octo/demo#7", "--pr", "8", "--once"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match --pr=8")
}

// TestMonitorShorthandSelectorRejectsUnknownForm checks the error message
// names the accepted selector forms, so a reader who typed something else
// knows what to try next.
func TestMonitorShorthandSelectorRejectsUnknownForm(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"octo/demo#7x", "--once"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner/repo#number")
}

// TestDraftStatusShorthandSelector covers a subcommand: the draft verbs take
// the same selector, and their own argument parsing must not mistake the
// shorthand for a bare number.
func TestDraftStatusShorthandSelector(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{}
	fake.restFunc = func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		switch path {
		case "repos/octo/demo":
			return assignJSON(result, map[string]interface{}{"full_name": "octo/demo"})
		case "repos/octo/demo/pulls/5":
			return assignJSON(result, map[string]interface{}{"node_id": "PR_node"})
		default:
			return errors.New("unexpected path")
		}
	}
	fake.graphqlFunc = func(query string, variables map[string]interface{}, result interface{}) error {
		if !strings.Contains(query, "PullRequestStatus") {
			return errors.New("unexpected query")
		}
		assert.Equal(t, "octo", variables["owner"])
		assert.Equal(t, "demo", variables["repo"])
		assert.Equal(t, 5, variables["number"])
		return assignJSON(result, map[string]interface{}{
			"repository": map[string]interface{}{
				"pullRequest": map[string]interface{}{
					"number":  5,
					"title":   "Test PR",
					"isDraft": false,
				},
			},
		})
	}
	apiClientFactory = func(host string) ghcli.API { return fake }

	cmd := newDraftStatusCommand(&backendOptions{})
	cmd.SetArgs([]string{"octo/demo#5"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)

	err := cmd.Execute()
	require.NoError(t, err)

	var output map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	assert.Equal(t, float64(5), output["pr_number"])
}

// TestCommentsReplyShorthandSelector covers the comments verb, which
// resolves the pull request before the reply mutation.
func TestCommentsReplyShorthandSelector(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{}
	fake.restFunc = func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		if path == "repos/octo/demo/pulls/5" {
			return assignJSON(result, map[string]interface{}{"node_id": "PR_node"})
		}
		return errors.New("unexpected path: " + path)
	}
	fake.graphqlFunc = func(query string, variables map[string]interface{}, result interface{}) error {
		switch {
		case strings.Contains(query, "AddPullRequestReviewThreadReply"):
			return assignJSON(result, map[string]interface{}{
				"addPullRequestReviewThreadReply": map[string]interface{}{
					"comment": map[string]interface{}{
						"id":          "PRRC_reply",
						"body":        "ack",
						"publishedAt": "2025-12-03T10:00:00Z",
						"author":      map[string]interface{}{"login": "octocat"},
					},
				},
			})
		case strings.Contains(query, "PullRequestReviewCommentDetails"):
			return assignJSON(result, map[string]interface{}{
				"node": map[string]interface{}{
					"id":         "PRRC_reply",
					"databaseId": 101,
					"body":       "ack",
					"diffHunk":   "@@ -10,5 +10,7 @@",
					"path":       "internal/service.go",
					"url":        "https://example.com/comment",
					"createdAt":  "2025-12-03T10:00:00Z",
					"updatedAt":  "2025-12-03T10:05:00Z",
					"author":     map[string]interface{}{"login": "octocat"},
				},
			})
		case strings.Contains(query, "PullRequestReviewThreadDetails"):
			return assignJSON(result, map[string]interface{}{
				"node": map[string]interface{}{
					"id":         "PRRT_thread",
					"isResolved": false,
					"isOutdated": false,
				},
			})
		default:
			return errors.New("unexpected query")
		}
	}
	apiClientFactory = func(host string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"comments", "reply", "octo/demo#5", "--thread-id", "PRRT_thread", "--body", "ack"})

	err := root.Execute()
	require.NoError(t, err)
	assert.Empty(t, stderr.String())

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &payload))
	assert.Equal(t, "PRRC_reply", payload["comment_node_id"])
}

// TestReviewViewShorthandSelector covers the review view verb, which
// resolves the pull request through the report service rather than the
// monitor's own path.
func TestReviewViewShorthandSelector(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &fakeViewAPI{payload: viewResponse, t: t}
	apiClientFactory = func(host string) ghcli.API { return fake }

	root := newRootCommand()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"review", "view", "octo/demo#51"})

	require.NoError(t, root.Execute())
	assert.Equal(t, "octo", fake.variables["owner"])
	assert.Equal(t, "demo", fake.variables["name"])
	assert.Equal(t, 51, fake.variables["number"])
}

// TestThreadsListShorthandSelector covers the threads verbs, which resolve
// the pull request over REST before listing its threads.
func TestThreadsListShorthandSelector(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{}
	fake.restFunc = func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		assert.Equal(t, "GET", method)
		switch path {
		case "repos/octo/demo":
			return assignJSON(result, map[string]interface{}{"full_name": "octo/demo"})
		case "repos/octo/demo/pulls/5":
			return assignJSON(result, map[string]interface{}{"node_id": "PR_node"})
		default:
			return errors.New("unexpected path: " + path)
		}
	}
	fake.graphqlFunc = func(query string, variables map[string]interface{}, result interface{}) error {
		if !strings.Contains(query, "reviewThreads") {
			return errors.New("unexpected query")
		}
		return assignJSON(result, map[string]interface{}{
			"node": map[string]interface{}{
				"reviewThreads": map[string]interface{}{
					"nodes": []map[string]interface{}{},
					"pageInfo": map[string]interface{}{
						"hasNextPage": false,
						"endCursor":   "",
					},
				},
			},
		})
	}
	apiClientFactory = func(host string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"threads", "list", "octo/demo#5"})

	err := root.Execute()
	require.NoError(t, err)
	assert.Empty(t, stderr.String())

	var payload []map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &payload))
	assert.Empty(t, payload)
}
