package cmd

import (
	"bytes"

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
// TestMonitorShorthandSelector covers the OWNER/REPO#NUMBER shorthand on the
// default command: it names its own repository, so it needs no -R and no git
// context, it resolves to the same identity as -R owner/repo NUMBER, and it
// carries no host of its own.
func TestMonitorShorthandSelector(t *testing.T) {
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
		require.Contains(t, query, "MonitorPR")
		owner, _ := variables["owner"].(string)
		repo, _ := variables["repo"].(string)
		number, _ := variables["number"].(int)
		seen = append(seen, target{owner, repo, number})
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	run := func(args []string) string {
		root := newRootCommand()
		stdout := &bytes.Buffer{}
		root.SetOut(stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		require.NoError(t, root.Execute())
		return stdout.String()
	}

	// The shorthand alone names the target.
	out := run([]string{"octo/demo#7", "--once"})
	require.Len(t, seen, 1)
	assert.Equal(t, target{owner: "octo", repo: "demo", number: 7}, seen[0])
	assert.Contains(t, out, `"pr_label":"octo/demo#7"`)

	// The shorthand and -R owner/repo NUMBER name the same target.
	run([]string{"-R", "octo/demo", "7", "--once"})
	require.Len(t, seen, 2)
	assert.Equal(t, seen[0], seen[1], "the shorthand and -R owner/repo NUMBER must name the same target")
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

// TestMonitorShorthandSelectorWithPrFlag covers the --pr consistency rule:
// a matching --pr is accepted, a mismatching one is rejected — the same
// rule a URL selector follows. It also covers the error message for a
// malformed shorthand, which names the accepted forms.
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

	// A malformed shorthand is rejected, and the error names the accepted
	// forms so a reader who typed something else knows what to try next.
	root = newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"octo/demo#7x", "--once"})
	err = root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner/repo#number")
}

// TestShorthandSelectorOnEveryVerb covers the verbs that take a pull
// request selector: each one must resolve the OWNER/REPO#NUMBER shorthand to
// the same target the -R owner/repo NUMBER form names.
func TestShorthandSelectorOnEveryVerb(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	// The pull request the shorthand must resolve to.
	const (
		wantOwner  = "octo"
		wantRepo   = "demo"
		wantNumber = 5
	)

	// restPaths answers the REST reads the verbs make to canonicalize the
	// pull request; gotPath records the one that was asked for.
	var gotPath string
	restFunc := func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		gotPath = path
		switch path {
		case "repos/octo/demo":
			return assignJSON(result, map[string]interface{}{"full_name": "octo/demo"})
		case "repos/octo/demo/pulls/5":
			return assignJSON(result, map[string]interface{}{"node_id": "PR_node"})
		default:
			return errors.New("unexpected path: " + path)
		}
	}

	// graphqlFunc answers every query the verbs make, and records the
	// pull request variables the query carried.
	var gotOwner, gotRepo string
	var gotNumber int
	graphqlFunc := func(query string, variables map[string]interface{}, result interface{}) error {
		if owner, ok := variables["owner"].(string); ok {
			gotOwner = owner
		}
		if repo, ok := variables["repo"].(string); ok {
			gotRepo = repo
		}
		if name, ok := variables["name"].(string); ok {
			gotRepo = name
		}
		if number, ok := variables["number"].(int); ok {
			gotNumber = number
		}
		switch {
		case strings.Contains(query, "PullRequestStatus"):
			return assignJSON(result, map[string]interface{}{
				"repository": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"number":  wantNumber,
						"title":   "Test PR",
						"isDraft": false,
					},
				},
			})
		case strings.Contains(query, "query Report("):
			// The report query: it needs a pull request to exist. It
			// queries reviewThreads too, so it is matched first.
			return assignJSON(result, map[string]interface{}{
				"repository": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"reviews":       map[string]interface{}{"nodes": []interface{}{}},
						"reviewThreads": map[string]interface{}{"nodes": []interface{}{}},
					},
				},
			})
		case strings.Contains(query, "reviewThreads"):
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
			return errors.New("unexpected query: " + query)
		}
	}

	tests := []struct {
		name string
		args []string
		// proof says how this verb reaches the pull request: through the
		// GraphQL variables, or through a REST read of the pull request.
		proof string
	}{
		{
			name:  "draft status",
			args:  []string{"draft", "status", "octo/demo#5"},
			proof: "graphql",
		},
		{
			name:  "threads list",
			args:  []string{"threads", "list", "octo/demo#5"},
			proof: "rest",
		},
		{
			name: "comments reply",
			args: []string{"comments", "reply", "octo/demo#5", "--thread-id", "PRRT_thread", "--body", "ack"},
			// The comments service posts to the thread the --thread-id
			// names and never reads the pull request, so the proof that the
			// shorthand was accepted is that the command reached the
			// mutation and answered.
			proof: "accepted",
		},
		{
			name:  "review view",
			args:  []string{"review", "view", "octo/demo#5"},
			proof: "graphql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOwner, gotRepo, gotNumber, gotPath = "", "", 0, ""
			fake := &commandFakeAPI{restFunc: restFunc, graphqlFunc: graphqlFunc}
			apiClientFactory = func(host string) ghcli.API { return fake }

			root := newRootCommand()
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			root.SetOut(stdout)
			root.SetErr(stderr)
			root.SetArgs(tt.args)

			require.NoError(t, root.Execute())
			assert.Empty(t, stderr.String())

			switch tt.proof {
			case "graphql":
				// The pull request the query asked for must be the one the
				// shorthand names.
				assert.Equal(t, wantOwner, gotOwner)
				assert.Equal(t, wantRepo, gotRepo)
				assert.Equal(t, wantNumber, gotNumber)
			case "rest":
				// A verb that reads the pull request over REST must have
				// reached the repository and the pull request the shorthand
				// named.
				assert.Equal(t, "repos/octo/demo/pulls/5", gotPath)
			case "accepted":
				// The command answered, so the shorthand was accepted.
				assert.NotEmpty(t, stdout.String())
			default:
				t.Fatalf("unknown proof %q", tt.proof)
			}
		})
	}
}
