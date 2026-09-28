package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stuckSource sends its updates and then keeps the stream open until the
// caller goes away. It ignores WatchOptions.Timeout, the way a sub-daemon
// that lost its broker connection keeps a watch open past the deadline
// (issue #127): only the client can end this watch on time.
type stuckSource struct {
	updates []backend.Update
}

func (s *stuckSource) Watch(ctx context.Context, t backend.Target, _ backend.WatchOptions) (<-chan backend.Update, error) {
	ch := make(chan backend.Update, len(s.updates))
	for _, u := range s.updates {
		u.Target = t
		ch <- u
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// timeoutLine is the subset of a rendered notification the timeout tests
// assert on.
type timeoutLine struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// lastLine parses the final NDJSON line of a monitor run's stdout.
func lastLine(t *testing.T, stdout string) timeoutLine {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.NotEmpty(t, lines)
	var n timeoutLine
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &n), "line not valid json: %s", lines[len(lines)-1])
	return n
}

// runStuckWatch runs a monitor against a stuckSource and returns its stdout
// and error. It fails the test when the watch outlives its --timeout by more
// than a few seconds.
func runStuckWatch(t *testing.T, updates []backend.Update, args ...string) (string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR},
		Source: &stuckSource{updates: updates},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"7", "-R", "o/r"}, args...))

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	select {
	case err := <-done:
		return stdout.String(), err
	case <-time.After(10 * time.Second):
		t.Fatal("the watch ran past its --timeout: the deadline is not enforced by the client")
		return "", nil
	}
}

// brokerLost is the degraded update a sub-daemon sends when its broker
// connection drops.
func brokerLost(at time.Time) backend.Update {
	return backend.Update{
		Event: backend.Event{
			Type:            backend.EventDegraded,
			DegradedSurface: "broker",
			DegradedMessage: "broker connection lost: connection lost: websocket: close 1005 (no status)",
		},
		At: at,
	}
}

// TestMonitorTimeoutIsAHardDeadline is the issue-#127 contract: --timeout
// ends the watch at the deadline even when the backend keeps the stream
// open after losing its broker. The last line says the timeout was reached
// while the watch was degraded, so the caller knows to read over REST.
func TestMonitorTimeoutIsAHardDeadline(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 3, 4, 0, time.UTC)
	stdout, err := runStuckWatch(t, []backend.Update{
		{Event: backend.Event{Type: backend.EventFirstPoll}, At: at},
		brokerLost(at),
	}, "--timeout", "1")
	require.NoError(t, err, "a watch that reaches its timeout ends normally")

	last := lastLine(t, stdout)
	assert.Equal(t, "timeout", last.Type)
	assert.Contains(t, last.Message, "--timeout 1s reached")
	assert.Contains(t, last.Message, "degraded")
	assert.Contains(t, last.Message, "broker connection lost")
	assert.Contains(t, last.Message, "REST")
}

// TestMonitorTimeoutOnAHealthyWatch pins the plain outcome: a watch that saw
// no degradation, or saw one recover, reports the timeout without a warning.
// --events timeout also proves the new kind passes the allowlist.
func TestMonitorTimeoutOnAHealthyWatch(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 3, 4, 0, time.UTC)
	recovered := backend.Update{
		Event: backend.Event{
			Type:   backend.EventDegraded,
			Notice: "✅ reconnected to daemon; resuming the watch from where it left off",
		},
		At: at.Add(time.Second),
	}
	cases := map[string][]backend.Update{
		"never degraded": {{Event: backend.Event{Type: backend.EventFirstPoll}, At: at}},
		"recovered":      {brokerLost(at), recovered},
	}
	for name, updates := range cases {
		t.Run(name, func(t *testing.T) {
			stdout, err := runStuckWatch(t, updates, "--timeout", "1", "--events", "timeout")
			require.NoError(t, err)
			assert.Equal(t, []string{"timeout"}, notificationTypes(t, stdout))
			last := lastLine(t, stdout)
			assert.Contains(t, last.Message, "--timeout 1s reached")
			assert.NotContains(t, last.Message, "degraded")
		})
	}
}

// TestMonitorUntilTimeoutStillExitsTwo keeps the --until contract: a watch
// whose deadline passed before any member fired is "not met" (exit 2), and
// the timeout line still tells the caller why the watch ended.
func TestMonitorUntilTimeoutStillExitsTwo(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 3, 4, 0, time.UTC)
	stdout, err := runStuckWatch(t, []backend.Update{brokerLost(at)},
		"--timeout", "1", "--until", "merged")
	require.ErrorIs(t, err, errUntilNotMet)
	assert.Equal(t, "timeout", lastLine(t, stdout).Type)
}

// TestMonitorStreamEndingEarlyIsNotATimeout is the companion: a stream that
// closes well before the deadline did not time out, so no timeout line is
// written.
func TestMonitorStreamEndingEarlyIsNotATimeout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 9, 28, 12, 3, 4, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{updates: []backend.Update{
			{Event: backend.Event{Type: backend.EventFirstPoll}, At: at},
		}},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--timeout", "60"})
	require.NoError(t, root.Execute())
	assert.Equal(t, []string{"first-poll"}, notificationTypes(t, stdout.String()))
}

// TestMonitorUntilRejectsTimeoutKind pins the README's rule that --timeout
// is a maximum watch time, never a completion condition: "timeout" is not a
// valid --until member.
func TestMonitorUntilRejectsTimeoutKind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "merged,timeout", "--timeout", "60"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--timeout")
}
