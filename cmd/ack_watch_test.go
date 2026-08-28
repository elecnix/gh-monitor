package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ackPRWithThread builds an OPEN PR with one unacknowledged general comment
// and one unresolved thread whose last comment carries no reaction — the two
// shapes the eyes-on-notification hook acknowledges.
func ackPRWithThread() obj {
	comment := func(id string) obj {
		return obj{"id": id, "body": "fix this", "author": obj{"login": "reviewer"}, "createdAt": "2026-01-01T00:00:00Z", "reactionGroups": []interface{}{}}
	}
	return obj{
		"repository": obj{
			"pullRequest": obj{
				"state":     "OPEN",
				"merged":    false,
				"mergeable": "MERGEABLE",
				"comments":  obj{"nodes": []interface{}{comment("IC_general1")}},
				"reviewThreads": obj{"nodes": []interface{}{obj{
					"id": "PRRT_1", "isResolved": false, "isOutdated": false, "path": "main.go",
					"comments": obj{"nodes": []interface{}{comment("PRRC_first")}},
				}}},
				"commits": obj{"nodes": []interface{}{obj{"commit": obj{
					"oid": "abcdef1234",
					"checkSuites": obj{"nodes": []interface{}{obj{
						"app":       obj{"name": "CI"},
						"checkRuns": obj{"nodes": []interface{}{obj{"name": "build", "conclusion": "SUCCESS"}}},
					}}},
				}}}},
			},
		},
	}
}

// runAckWatch executes the monitor command with the given extra args against a
// fake GraphQL API serving ackPRWithThread, after writing prefsJSON to the
// preferences file, and returns stdout, stderr, and the list of addReaction
// subject IDs the command made.
func runAckWatch(t *testing.T, prefsJSON string, extraArgs ...string) (stdout, stderr string, reacted []string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if prefsJSON != "" {
		cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gh-monitor")
		require.NoError(t, os.MkdirAll(cfgDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "preferences.json"), []byte(prefsJSON), 0o644))
	}
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	var reactTo []string
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		// The monitor query carries the snapshot; the reaction mutation is
		// distinguishable by its operation name.
		if strings.Contains(query, "addReaction") {
			<-mu
			reactTo = append(reactTo, variables["subjectId"].(string))
			mu <- struct{}{}
			return nil
		}
		return assignJSON(result, ackPRWithThread())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetArgs(append([]string{"7", "-R", "o/r", "--once"}, extraArgs...))
	require.NoError(t, root.Execute())

	<-mu
	reacted = reactTo
	mu <- struct{}{}
	return outBuf.String(), errBuf.String(), reacted
}

// TestMonitorOnceReactsEyesOnDeliverable is the acceptance case: with the
// default preferences, a watch that delivers new thread and general-comment
// notifications reacts 👀 to each comment it delivered, so humans on the PR
// see the notification was received.
func TestMonitorOnceReactsEyesOnDeliverable(t *testing.T) {
	stdout, _, reacted := runAckWatch(t, "")
	assert.Contains(t, stdout, `"new-unresolved-threads"`, "precondition: a thread notification is delivered")
	assert.Contains(t, stdout, `"new-general-comments"`, "precondition: a comment notification is delivered")
	assert.ElementsMatch(t, []string{"PRRC_first", "IC_general1"}, reacted,
		"each delivered comment must get exactly one 👀 reaction")
}

// TestMonitorOnceReactionFailureDoesNotBreakWatch pins the degraded-not-lost
// contract: a failing reaction mutation costs one stderr line; the
// notification still goes out and the watch keeps running.
func TestMonitorOnceReactionFailureDoesNotBreakWatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		if strings.Contains(query, "addReaction") {
			return assert.AnError
		}
		return assignJSON(result, ackPRWithThread())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetArgs([]string{"7", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	assert.Contains(t, outBuf.String(), `"new-unresolved-threads"`,
		"the notification must still be delivered when the reaction fails")
	assert.Contains(t, errBuf.String(), "eyes-on-notify",
		"the reaction failure must surface as a prefixed stderr warning")
}

// TestMonitorOnceReactionsRespectEventsFilter pins the emit-boundary rule: a
// kind suppressed by --events is not delivered, so its comments are not
// reacted to either — the hook sits above the same boundary.
func TestMonitorOnceReactionsRespectEventsFilter(t *testing.T) {
	_, _, reacted := runAckWatch(t, "", "--events", "first-poll")
	assert.Empty(t, reacted,
		"suppressed kinds must not trigger reactions; only delivered notifications are acknowledged")
}

// TestMonitorOnceNoReactionWhenPrefOff pins the off switch: reactOnNotify
// false in preferences.json disables the hook entirely.
func TestMonitorOnceNoReactionWhenPrefOff(t *testing.T) {
	_, _, reacted := runAckWatch(t, `{"reactOnNotify": false}`)
	assert.Empty(t, reacted)
}
