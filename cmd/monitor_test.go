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

// openPRWithFailingCheck is a monitor GraphQL payload for an open PR with one
// failing check run.
func openPRWithFailingCheck() obj {
	return obj{
		"repository": obj{
			"pullRequest": obj{
				"state":     "OPEN",
				"merged":    false,
				"mergeable": "MERGEABLE",
				"commits": obj{"nodes": []interface{}{obj{"commit": obj{
					"oid": "abcdef1234",
					"checkSuites": obj{"nodes": []interface{}{obj{
						"app":       obj{"name": "CI"},
						"checkRuns": obj{"nodes": []interface{}{obj{"name": "build", "conclusion": "FAILURE"}}},
					}}},
				}}}},
			},
		},
	}
}

func TestMonitorOnceEmitsNDJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorPR")
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	// Every line is valid JSON.
	var firstPollSeen, failingSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		switch n["type"] {
		case "first-poll":
			firstPollSeen = true
		case "new-failing-checks":
			failingSeen = true
			assert.Equal(t, "o/r#7", n["pr_label"])
		}
	}
	assert.True(t, firstPollSeen, "expected a first-poll event")
	assert.True(t, failingSeen, "expected a new-failing-checks event")
}

func TestMonitorOnceTextMode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once", "--text"})
	require.NoError(t, root.Execute())

	out := stdout.String()
	assert.NotContains(t, out, `"type":`) // not JSON
	// The PR label is OSC-8 linkified in --text mode; the surrounding rendered
	// text is unchanged.
	assert.Contains(t, out, "\x1b]8;;https://github.com/o/r/pull/7\x1b\\o/r#7\x1b]8;;\x1b\\")
	assert.Contains(t, out, "📡 Monitoring ")
	assert.Contains(t, out, "❌ Failing CI checks on ")
	assert.Contains(t, out, ": build")
}

func TestMonitorRequiresPR(t *testing.T) {
	// --repo alone is now valid for repo monitoring, but a bare command with no
	// flags at all still requires a PR number or other target.
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull request number or URL is required")
}

func TestMonitorOnceEmitsNDJSON_DefaultCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorPR")
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	var firstPollSeen, failingSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		switch n["type"] {
		case "first-poll":
			firstPollSeen = true
		case "new-failing-checks":
			failingSeen = true
			assert.Equal(t, "o/r#7", n["pr_label"])
		}
	}
	assert.True(t, firstPollSeen, "expected a first-poll event")
	assert.True(t, failingSeen, "expected a new-failing-checks event")
}

func TestMonitorRepoOnceEmitsNDJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	t.Setenv("GH_VIEWER", "viewer")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorReadiness")
		return assignJSON(result, obj{
			"repository": obj{
				"pullRequests": obj{
					"totalCount": 1,
					"nodes": []interface{}{obj{
						"number":           1,
						"state":            "OPEN",
						"mergeable":        "MERGEABLE",
						"mergeStateStatus": "CLEAN",
						"author":           obj{"login": "viewer"},
						"reviews":          obj{"nodes": []interface{}{}},
						"commits": obj{"nodes": []interface{}{obj{
							"commit": obj{
								"oid":             "abc1234",
								"messageHeadline": "test",
								"message":         "",
								"authors":         obj{"nodes": []interface{}{}},
								"checkSuites": obj{
									"totalCount": 0,
									"nodes":      []interface{}{},
								},
								"status": nil,
							},
						}}},
					}},
				},
			},
		})
	}}
	// Mock the REST call for ruleset fetch (returns no rulesets).
	fake.restFunc = func(method, path string, params map[string]string, body, result interface{}) error {
		return assignJSON(result, []interface{}{})
	}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 1)
	var readinessSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		if n["type"] == "readiness" {
			readinessSeen = true
			msg, _ := n["message"].(string)
			assert.Contains(t, msg, "open=1")
			assert.Contains(t, msg, "staging=success")
		}
	}
	assert.True(t, readinessSeen, "expected a readiness event")
}

func TestRootRejectsTooManyArgs(t *testing.T) {
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "8"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "accepts at most")
}

// ---------------------------------------------------------------------------
// Ref / commit / issue monitoring tests
// ---------------------------------------------------------------------------

func refWithFailingCheck() obj {
	return obj{
		"repository": obj{
			"ref": obj{
				"target": obj{
					"oid":             "abcdef1234",
					"messageHeadline": "fix: stuff",
					"authors": obj{"nodes": []interface{}{obj{
						"name": "test",
						"user": obj{"login": "test"},
					}}},
					"checkSuites": obj{"nodes": []interface{}{obj{
						"app":       obj{"name": "CI"},
						"checkRuns": obj{"nodes": []interface{}{obj{"name": "build", "conclusion": "FAILURE"}}},
					}}},
				},
			},
		},
	}
}

func commitWithFailingCheck() obj {
	return obj{
		"repository": obj{
			"object": obj{
				"oid":             "abcdef1234",
				"messageHeadline": "fix: stuff",
				"authors": obj{"nodes": []interface{}{obj{
					"name": "test",
					"user": obj{"login": "test"},
				}}},
				"checkSuites": obj{"nodes": []interface{}{obj{
					"app":       obj{"name": "CI"},
					"checkRuns": obj{"nodes": []interface{}{obj{"name": "build", "conclusion": "FAILURE"}}},
				}}},
			},
		},
	}
}

func issueWithComment() obj {
	return obj{
		"repository": obj{
			"issue": obj{
				"state": "OPEN",
				"title": "bug report",
				"comments": obj{"nodes": []interface{}{obj{
					"id":     "IC_kw",
					"body":   "please fix",
					"author": obj{"login": "alice"},
				}}},
			},
		},
	}
}

func TestMonitorOnceRefEmitsNDJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorRef")
		return assignJSON(result, refWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--ref", "main", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	var firstPollSeen, failingSeen, commitSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		switch n["type"] {
		case "first-poll":
			firstPollSeen = true
		case "new-failing-checks":
			failingSeen = true
		case "new-commit":
			commitSeen = true
		}
	}
	assert.True(t, firstPollSeen, "expected a first-poll event")
	assert.True(t, failingSeen, "expected a new-failing-checks event")
	assert.True(t, commitSeen, "expected a new-commit event")
}

func TestMonitorOnceCommitEmitsNDJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorCommit")
		return assignJSON(result, commitWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--commit", "abc123def", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	var firstPollSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		if n["type"] == "first-poll" {
			firstPollSeen = true
		}
	}
	assert.True(t, firstPollSeen, "expected a first-poll event")
}

func TestMonitorOnceIssueEmitsNDJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		// The eyes-on-notify hook issues its own addReaction mutation after the
		// snapshot query; only the snapshot must carry the MonitorIssue query.
		if strings.Contains(query, "addReaction") {
			return nil
		}
		require.Contains(t, query, "MonitorIssue")
		return assignJSON(result, issueWithComment())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--issue", "42", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	var firstPollSeen, commentSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		switch n["type"] {
		case "first-poll":
			firstPollSeen = true
		case "issue-new-comment":
			commentSeen = true
		}
	}
	assert.True(t, firstPollSeen, "expected a first-poll event")
	assert.True(t, commentSeen, "expected an issue-new-comment event")
}

func TestMonitorRefRequiresRef(t *testing.T) {
	// Empty --ref with no other target triggers repo monitoring (valid).
	// Ref monitoring requires a non-empty ref value.
	// This test verifies that --ref and --issue together are mutually exclusive.
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--ref", "main", "--issue", "42", "-R", "o/r"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestMonitorRefWithRepo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		require.Contains(t, query, "MonitorRef")
		return assignJSON(result, refWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--ref", "main", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())
	assert.Contains(t, stdout.String(), "first-poll")
}

// cleanTarget is the commit payload body shared by the ref and --baseline
// fixtures (clean: no failing checks).
func cleanTarget(oid string) obj {
	return obj{
		"oid":             oid,
		"messageHeadline": "fix: stuff",
		"authors":         obj{"nodes": []interface{}{}},
		"checkSuites":     obj{"nodes": []interface{}{}},
	}
}

// baselineRefFixture is a clean (no failing checks) ref payload for --baseline tests.
func baselineRefFixture(oid string) obj {
	return obj{"repository": obj{"ref": obj{"target": cleanTarget(oid)}}}
}

// baselineCommitFixture is the commit payload answering the --baseline OID lookup.
func baselineCommitFixture(oid string) obj {
	return obj{"repository": obj{"object": cleanTarget(oid)}}
}

const baselineObservedOID = "aaaaaaabbbbccccdddd00000000000000000000"

func TestMonitorOnceWithBaseline(t *testing.T) {
	t.Run("baseline equal to remote head suppresses everything", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("GH_HOST", "")
		originalFactory := apiClientFactory
		defer func() { apiClientFactory = originalFactory }()

		fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			if strings.Contains(query, "MonitorCommit") {
				require.Equal(t, baselineObservedOID, variables["oid"])
				return assignJSON(result, baselineCommitFixture(baselineObservedOID))
			}
			require.Contains(t, query, "MonitorRef")
			return assignJSON(result, baselineRefFixture(baselineObservedOID))
		}}
		apiClientFactory = func(string) ghcli.API { return fake }

		root := newRootCommand()
		stdout := &bytes.Buffer{}
		root.SetOut(stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"--ref", "main", "-R", "o/r", "--once", "--baseline", baselineObservedOID})
		require.NoError(t, root.Execute())
		assert.Empty(t, strings.TrimSpace(stdout.String()), "nothing changed since the observed OID: no events")
	})

	t.Run("push landing after observation is delivered on first poll", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("GH_HOST", "")
		originalFactory := apiClientFactory
		defer func() { apiClientFactory = originalFactory }()

		advancedOID := "bbbbbbbaaaacccdddd0000000000000000000000"
		fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			if strings.Contains(query, "MonitorCommit") {
				return assignJSON(result, baselineCommitFixture(baselineObservedOID))
			}
			return assignJSON(result, baselineRefFixture(advancedOID))
		}}
		apiClientFactory = func(string) ghcli.API { return fake }

		root := newRootCommand()
		stdout := &bytes.Buffer{}
		root.SetOut(stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"--ref", "main", "-R", "o/r", "--once", "--baseline", baselineObservedOID})
		require.NoError(t, root.Execute())
		assert.Contains(t, stdout.String(), "new-commit", "the push since the observed OID must be delivered")
		assert.NotContains(t, stdout.String(), "first-poll", "a seeded watch is not a first poll")
	})

	t.Run("a typo'd SHA fails loudly before any watch starts", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("GH_HOST", "")
		apiCalls := 0
		originalFactory := apiClientFactory
		defer func() { apiClientFactory = originalFactory }()

		fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			apiCalls++
			if strings.Contains(query, "MonitorCommit") {
				// GitHub answers a nonexistent object with no data.
				return assignJSON(result, obj{"repository": obj{"object": nil}})
			}
			return errors.New("unexpected MonitorRef call: watch must not start past a failed baseline")
		}}
		apiClientFactory = func(string) ghcli.API { return fake }

		root := newRootCommand()
		stdout := &bytes.Buffer{}
		root.SetOut(stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"--ref", "main", "-R", "o/r", "--once", "--baseline", "zzzzzzznotfound"})
		err := root.Execute()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "unexpected MonitorRef", "the watch must never start when the baseline is bad")
	})
}

// expectMonitorError runs the monitor command with args and asserts it fails
// with want in the error message. Shared by the flag-validation tables.
func expectMonitorError(t *testing.T, args []string, want string) {
	t.Helper()
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), want)
}

func TestMonitorBaselineFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "baseline without ref",
			args: []string{"--baseline", "3f9c2ab", "-R", "o/r"},
			want: "--baseline requires --ref",
		},
		{
			name: "baseline with commit target",
			args: []string{"--baseline", "3f9c2ab", "--commit", "abcdef1", "-R", "o/r"},
			want: "--baseline requires --ref",
		},
		{
			name: "baseline with instance",
			args: []string{"--baseline", "3f9c2ab", "--ref", "main", "--instance", "main-watch", "-R", "o/r"},
			want: "--instance",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectMonitorError(t, tt.args, tt.want)
		})
	}
}

func TestMonitorMutuallyExclusiveTargets(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "ref and pr",
			args: []string{"--ref", "main", "-R", "o/r", "7"},
			want: "mutually exclusive",
		},
		{
			name: "ref and commit",
			args: []string{"--ref", "main", "--commit", "abc", "-R", "o/r"},
			want: "mutually exclusive",
		},
		{
			name: "ref and issue",
			args: []string{"--ref", "main", "--issue", "42", "-R", "o/r"},
			want: "mutually exclusive",
		},
		{
			name: "issue and pr",
			args: []string{"--issue", "42", "-R", "o/r", "7"},
			want: "mutually exclusive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectMonitorError(t, tt.args, tt.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Workflow-run monitoring tests
// ---------------------------------------------------------------------------

func workflowRunJSON(status, conclusion string) obj {
	return obj{
		"id":            30433642,
		"name":          "deploy",
		"display_title": "Deploy to prod",
		"event":         "workflow_dispatch",
		"status":        status,
		"conclusion":    conclusion,
		"head_branch":   "main",
		"head_sha":      "abcdef1234567890",
		"html_url":      "https://github.com/o/r/actions/runs/30433642",
		"run_number":    42,
	}
}

func TestMonitorOnceRunEmitsNDJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{restFunc: func(method, path string, params map[string]string, body interface{}, result interface{}) error {
		assert.Equal(t, "GET", method)
		assert.Equal(t, "repos/o/r/actions/runs/30433642", path)
		return assignJSON(result, workflowRunJSON("completed", "success"))
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--run-id", "30433642", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	var firstPollSeen, completedSeen bool
	for _, ln := range lines {
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		switch n["type"] {
		case "first-poll":
			firstPollSeen = true
		case "run-completed":
			completedSeen = true
			assert.Equal(t, "success", n["conclusion"])
			assert.EqualValues(t, 30433642, n["run_id"])
		}
	}
	assert.True(t, firstPollSeen, "expected a first-poll event")
	assert.True(t, completedSeen, "expected a run-completed event")
}

func TestMonitorRunIdZeroRequiresTarget(t *testing.T) {
	// --run-id 0 with --repo alone is now valid repo monitoring (run-id 0 = unset).
	// No target at all should still error.
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--run-id", "0"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull request number or URL is required")
}

func TestMonitorRunMutuallyExclusiveWithPR(t *testing.T) {
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--run-id", "30433642", "-R", "o/r", "7"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestMonitorOnceEventsFlagSuppressesUnlistedKinds confirms the --events /
// --only-events CLI flag threads the per-event-kind allowlist through to the
// emit boundary: with --events=new-failing-checks, the first-poll and
// ci-all-green notifications are suppressed and only new-failing-checks is
// emitted.
func TestMonitorOnceEventsFlagSuppressesUnlistedKinds(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once", "--events", "new-failing-checks"})
	require.NoError(t, root.Execute())

	var types []string
	for _, ln := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if ln == "" {
			continue
		}
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		types = append(types, n["type"].(string))
	}
	// Only the allowlisted kind may appear.
	for _, ty := range types {
		assert.Equal(t, "new-failing-checks", ty, "--events must suppress unlisted kinds; got %q", ty)
	}
	assert.Contains(t, types, "new-failing-checks", "the allowlisted kind must still be emitted")
	assert.NotContains(t, types, "first-poll", "first-poll is not in the allowlist and must be suppressed")
}

// TestMonitorEventsRejectsUnknownKind confirms an unknown event kind on the CLI
// is a hard error rather than silently ignored.
func TestMonitorEventsRejectsUnknownKind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once", "--events", "conflict,not-a-real-kind"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-real-kind")
}
