package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// greenPR builds a monitor GraphQL payload for an open PR whose single check
// run is already green and which carries no comments, threads, or conflicts —
// the shape whose first poll fires exactly one ci-all-green and nothing else.
func greenPR() obj {
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
						"checkRuns": obj{"nodes": []interface{}{obj{"name": "build", "conclusion": "SUCCESS"}}},
					}}},
				}}}},
			},
		},
	}
}

// untilNotification is the subset of a rendered notification the --until
// tests assert on.
type untilNotification struct {
	Type string `json:"type"`
}

// notificationTypes parses the NDJSON lines of a monitor run's stdout into the
// ordered list of notification types.
func notificationTypes(t *testing.T, stdout string) []string {
	t.Helper()
	var types []string
	for _, ln := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if ln == "" {
			continue
		}
		var n untilNotification
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		types = append(types, n.Type)
	}
	return types
}

// TestMonitorUntilFiresAndReportsTheTriggeringEvent is the exit-0 path: the
// first event in the stream whose kind is in the --until set is reported and
// the watch stops at the end of its batch. Neither update sets More, so each
// is a batch of its own and the second never prints.
func TestMonitorUntilFiresAndReportsTheTriggeringEvent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{
			updates: []backend.Update{
				{
					Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}},
					At:    at,
				},
				{
					Event: backend.Event{Type: backend.EventNewUnresolvedThreads},
					At:    at,
				},
			},
		},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "ci-all-green,new-failing-checks"})
	require.NoError(t, root.Execute())

	types := notificationTypes(t, stdout.String())
	// The triggering event is reported, then the watch stops: the second
	// update belongs to the next batch.
	require.Equal(t, []string{"new-failing-checks"}, types)
}

// TestMonitorUntilNotMetWhenStreamCoversNoMember is the exit-2 sentinel path:
// the stream ends without any member of the --until set firing, so the caller
// must be able to tell "condition met" (0) from "the watch ended first" (2).
func TestMonitorUntilNotMetWhenStreamCoversNoMember(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{
			updates: []backend.Update{
				{
					Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}},
					At:    at,
				},
				{
					Event:    backend.Event{Type: backend.EventMerged},
					At:       at,
					Terminal: true,
				},
			},
		},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "ci-all-green"})
	err := root.Execute()
	require.Error(t, err)
	require.ErrorIs(t, err, errUntilNotMet)
}

// TestMonitorUntilAlreadyGreenPRExitsZero pins the already-green interaction:
// a green PR emits ci-all-green on the first poll, so --until ci-all-green
// exits 0 almost immediately instead of waiting for a timeout.
func TestMonitorUntilAlreadyGreenPRExitsZero(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, greenPR())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once", "--until", "ci-all-green"})
	require.NoError(t, root.Execute())

	assert.Contains(t, notificationTypes(t, stdout.String()), "ci-all-green",
		"the triggering event must be reported before exiting")
}

// TestMonitorUntilMultiMemberSetFiresOnFirstMatchingMember: a multi-member
// set exits on the FIRST member to fire in the stream, not on any fixed
// member order.
func TestMonitorUntilMultiMemberSetFiresOnFirstMatchingMember(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{
			updates: []backend.Update{
				{
					Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}},
					At:    at,
				},
				{
					Event: backend.Event{Type: backend.EventConflict},
					At:    at,
				},
				// Below the trigger: must never be reported.
				{
					Event: backend.Event{Type: backend.EventNewUnresolvedThreads},
					At:    at,
				},
			},
		},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "ci-all-green,conflict"})
	require.NoError(t, root.Execute())

	types := notificationTypes(t, stdout.String())
	// new-failing-checks is emitted (it is not a member) and conflict is the
	// first member to fire; the next batch is never replayed.
	require.Equal(t, []string{"new-failing-checks", "conflict"}, types)
}

// blockingSource opens a watch that never delivers an update and never
// closes: the stream stays alive until the client gives up.
type blockingSource struct {
	watched chan backend.Target
}

func (s *blockingSource) Watch(ctx context.Context, t backend.Target, _ backend.WatchOptions) (<-chan backend.Update, error) {
	if s.watched != nil {
		select {
		case s.watched <- t:
		default:
		}
	}
	// Block until the server context ends (the client hung up), then close,
	// so the transport-level stream ends the way a real quiet target does.
	ch := make(chan backend.Update)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestMonitorUntilCtrlCIsNotConditionNotMet pins the Ctrl-C contract: a
// SIGINT during a watch whose --until set never fired is neither "condition
// met" nor "the watch ended on its own" — it is the user cancelling, so the
// command must exit as before (nil from Execute, exit 0 from ExecuteOrExit)
// and never report errUntilNotMet.
func TestMonitorUntilCtrlCIsNotConditionNotMet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	src := &blockingSource{watched: make(chan backend.Target, 1)}
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR},
		Source: src,
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "ci-all-green"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	// The signal handler is installed before the backend is attached, so once
	// the watch request reaches the source, SIGINT is guaranteed to cancel the
	// command's context instead of killing the test process.
	select {
	case <-src.watched:
	case <-time.After(10 * time.Second):
		t.Fatal("watch never started")
	}
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	select {
	case err := <-done:
		require.NoError(t, err)
		require.NotErrorIs(t, err, errUntilNotMet)
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end after SIGINT")
	}
}

// TestMonitorUntilRejectsUnknownKind mirrors TestMonitorEventsRejectsUnknownKind:
// a typo in --until is a loud error, not a condition that never fires.
func TestMonitorUntilRejectsUnknownKind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once", "--until", "ci-all-green,not-a-real-kind"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-real-kind")
}

// redPRWithBacklog builds an OPEN PR whose only check run failed and which
// already carries an unresolved review thread and a general comment. Its first
// poll diffs against an empty baseline, so the batch replays that backlog, and
// the diff orders new-failing-checks ahead of the thread and the comment.
func redPRWithBacklog() obj {
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
						"status":    "COMPLETED",
						"checkRuns": obj{"nodes": []interface{}{obj{"name": "build", "status": "COMPLETED", "conclusion": "FAILURE"}}},
					}}},
				}}}},
			},
		},
	}
}

// TestMonitorUntilPrintsTheWholeFirstPollBatch is issue #116: a --until
// member that fires partway through the first poll's batch must not drop the
// events the diff orders after it. The watch prints the whole batch, then
// exits on that same poll.
func TestMonitorUntilPrintsTheWholeFirstPollBatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		if strings.Contains(query, "addReaction") {
			return nil
		}
		return assignJSON(result, redPRWithBacklog())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once", "--until", "new-failing-checks"})
	require.NoError(t, root.Execute())

	types := notificationTypes(t, stdout.String())
	require.Contains(t, types, "new-failing-checks")
	assert.Contains(t, types, "new-unresolved-threads",
		"the thread follows the trigger in the same batch and must still print")
	assert.Contains(t, types, "new-general-comments",
		"the comment follows the trigger in the same batch and must still print")
}

// TestMonitorUntilExitsAtTheEndOfTheTriggeringBatch pins where the watch
// stops: every update the source marks as part of the trigger's batch
// (Update.More) prints, and the next batch never does.
func TestMonitorUntilExitsAtTheEndOfTheTriggeringBatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{
			updates: []backend.Update{
				// First batch: the trigger, then two events after it.
				{Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}}, At: at, More: true},
				{Event: backend.Event{Type: backend.EventNewUnresolvedThreads}, At: at, More: true},
				{Event: backend.Event{Type: backend.EventCheckAnnotations}, At: at},
				// Second batch: must never be reported.
				{Event: backend.Event{Type: backend.EventNewGeneralComments}, At: at},
			},
		},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "new-failing-checks"})
	require.NoError(t, root.Execute())

	require.Equal(t, []string{"new-failing-checks", "new-unresolved-threads", "check-annotations"},
		notificationTypes(t, stdout.String()))
}
