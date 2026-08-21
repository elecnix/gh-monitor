package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveTestBackend starts an external backend on a Unix socket and returns its
// endpoint. It stands in for a real out-of-process backend.
func serveTestBackend(t *testing.T, cfg remote.ServerConfig) string {
	t.Helper()
	// A short directory, not t.TempDir(): a Unix socket path is capped near
	// 104 bytes, and the per-test temp path built from the test's name blows
	// past that.
	dir, err := os.MkdirTemp("", "ghm")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = remote.Serve(context.Background(), conn, cfg)
			}()
		}
	}()
	return "unix:" + sock
}

// staticSource replays a fixed set of updates and closes.
type staticSource struct {
	updates []backend.Update
	watched chan backend.Target
}

func (s *staticSource) Watch(ctx context.Context, t backend.Target, _ backend.WatchOptions) (<-chan backend.Update, error) {
	if s.watched != nil {
		select {
		case s.watched <- t:
		default:
		}
	}
	ch := make(chan backend.Update, len(s.updates))
	for _, u := range s.updates {
		u.Target = t
		ch <- u
	}
	close(ch)
	return ch, nil
}

func TestBackendsCommandListsTheBuiltInBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(backendEndpointEnv, "")

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"backends"})
	require.NoError(t, root.Execute())

	out := stdout.String()
	assert.Contains(t, out, "gh")
	assert.Contains(t, out, "reader")
	// The Source it registers serves one-shot reads only; continuous watching
	// goes through the shared-poller daemon (issue #76).
	assert.Contains(t, out, "source")
	// It covers every kind for what it does register, so it reports "all"
	// rather than enumerating them.
	assert.Contains(t, out, "all")
}

func TestBackendsCommandShowsAnExternalBackendsPartialSurface(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR, backend.KindRun},
		Source: &staticSource{},
	})
	t.Setenv(backendEndpointEnv, endpoint)

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"backends", "--json"})
	require.NoError(t, root.Execute())

	var infos []backend.Info
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &infos))
	require.Len(t, infos, 2)

	byName := map[string]backend.Info{}
	for _, i := range infos {
		byName[i.Name] = i
	}

	// The external backend declared a Source for two kinds and nothing else,
	// so that is exactly what it is registered for — reads stay with gh.
	relay := byName["relay"]
	assert.Equal(t, []backend.Capability{backend.CapSource}, relay.Capabilities)
	assert.Equal(t, []backend.Kind{backend.KindPR, backend.KindRun}, relay.Kinds)

	gh := byName["gh"]
	assert.Contains(t, gh.Capabilities, backend.CapReader)
	assert.Nil(t, gh.Kinds, "the built-in backend covers every kind")
}

func TestMonitorStreamsFromAnExternalBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	watched := make(chan backend.Target, 1)
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{
			watched: watched,
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

	// The built-in backend must not be consulted at all for this target.
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API {
		return &commandFakeAPI{graphqlFunc: func(string, map[string]interface{}, interface{}) error {
			t.Error("the built-in backend polled GitHub even though an external backend covers this target")
			return nil
		}}
	}

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r"})
	require.NoError(t, root.Execute())

	// The target reached the backend intact.
	select {
	case tgt := <-watched:
		assert.Equal(t, backend.KindPR, tgt.Kind)
		assert.Equal(t, "o", tgt.Owner)
		assert.Equal(t, "r", tgt.Repo)
		assert.Equal(t, 7, tgt.Number)
	case <-time.After(5 * time.Second):
		t.Fatal("the external backend was never asked to watch")
	}

	// Its updates were rendered with this build's templates, not the backend's.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 2)
	var first, second monitor.Notification
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))

	assert.Equal(t, "new-failing-checks", first.Type)
	assert.Equal(t, "o/r#7", first.PRLabel)
	assert.NotEmpty(t, first.Message)
	assert.Equal(t, []string{"build"}, first.FailingChecks)
	assert.Equal(t, "merged", second.Type)
}

// TestMonitorUncoveredKindIsAHardError verifies the end-state behaviour after
// the in-process loops were deleted (issue #76): an external backend that
// covers only some kinds leaves the others to no one, and a watch for an
// uncovered kind fails loudly rather than silently doing nothing.
func TestMonitorUncoveredKindIsAHardError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	// The external backend covers pull requests only.
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR},
		Source: &staticSource{},
	})
	t.Setenv(backendEndpointEnv, endpoint)

	polled := false
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API {
		return &commandFakeAPI{
			graphqlFunc: func(query string, _ map[string]interface{}, result interface{}) error {
				polled = true
				return assignJSON(result, obj{"repository": obj{"issue": obj{
					"state": "CLOSED", "title": "t", "comments": obj{"nodes": []interface{}{}},
				}}})
			},
		}
	}

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--issue", "3", "-R", "o/r"})
	err := root.Execute()
	require.Error(t, err)
	assert.False(t, polled,
		"the built-in backend registers no Source: nothing may silently watch what no backend covers")
}

func TestMonitorRejectsAnUnknownPinnedBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(backendEndpointEnv, "")

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--backend", "nope"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestMonitorRejectsAMalformedBackendEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(backendEndpointEnv, "carrier-pigeon:somewhere")

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r"})
	err := root.Execute()
	// A mistyped endpoint has to fail at startup. Falling back silently would
	// leave the operator believing an external backend is watching when the
	// built-in one is, or nothing is.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carrier-pigeon")
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

// capturingThreads records the resolve it was asked to perform.
type capturingThreads struct{ resolved chan string }

func (c *capturingThreads) ListThreads(context.Context, backend.Target, backend.ThreadListOptions) ([]backend.Thread, error) {
	return []backend.Thread{{ThreadID: "PRRT_from_backend", Path: "x.go"}}, nil
}

func (c *capturingThreads) ViewThreads(context.Context, backend.Target, []string) ([]backend.ThreadWithComments, error) {
	return nil, nil
}

func (c *capturingThreads) ResolveThread(_ context.Context, _ backend.Target, ref backend.ThreadRef) (backend.ThreadResolution, error) {
	select {
	case c.resolved <- ref.ThreadID:
	default:
	}
	return backend.ThreadResolution{ThreadNodeID: ref.ThreadID, IsResolved: true}, nil
}

func (c *capturingThreads) UnresolveThread(context.Context, backend.Target, backend.ThreadRef) (backend.ThreadResolution, error) {
	return backend.ThreadResolution{}, nil
}

func TestThreadsResolveGoesToAnExternalBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	actor := &capturingThreads{resolved: make(chan string, 1)}
	t.Setenv(backendEndpointEnv, serveTestBackend(t, remote.ServerConfig{
		Name:    "relay",
		Kinds:   []backend.Kind{backend.KindPR},
		Threads: actor,
	}))

	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API {
		return &commandFakeAPI{graphqlFunc: func(string, map[string]interface{}, interface{}) error {
			t.Error("the built-in backend was called even though an external backend serves threads")
			return nil
		}}
	}

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"threads", "resolve", "7", "-R", "o/r", "--thread-id", "PRRT_abc"})
	require.NoError(t, root.Execute())

	select {
	case id := <-actor.resolved:
		assert.Equal(t, "PRRT_abc", id)
	case <-time.After(5 * time.Second):
		t.Fatal("the external backend was never asked to resolve")
	}
	assert.Contains(t, stdout.String(), "PRRT_abc")
}

func TestThreadsListGoesToAnExternalBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	t.Setenv(backendEndpointEnv, serveTestBackend(t, remote.ServerConfig{
		Name:    "relay",
		Kinds:   []backend.Kind{backend.KindPR},
		Threads: &capturingThreads{},
	}))

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"threads", "list", "7", "-R", "o/r"})
	require.NoError(t, root.Execute())

	assert.Contains(t, stdout.String(), "PRRT_from_backend")
}

func TestDraftStaysWithTheBuiltInBackendWhenNotDeclared(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	// The external backend serves threads only. Draft must still work, via gh.
	t.Setenv(backendEndpointEnv, serveTestBackend(t, remote.ServerConfig{
		Name:    "relay",
		Kinds:   []backend.Kind{backend.KindPR},
		Threads: &capturingThreads{},
	}))

	called := false
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API {
		return &commandFakeAPI{graphqlFunc: func(_ string, _ map[string]interface{}, result interface{}) error {
			called = true
			return assignJSON(result, obj{"repository": obj{"pullRequest": obj{
				"id": "PR_1", "number": 7, "isDraft": true, "title": "wip",
			}}})
		}}
	}

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"draft", "status", "7", "-R", "o/r"})
	require.NoError(t, root.Execute())

	assert.True(t, called, "draft was not declared by the external backend, so gh must serve it")
	assert.Contains(t, stdout.String(), "\"is_draft\":true")
}

func TestBackendsCommandListsMutationCapabilities(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(backendEndpointEnv, serveTestBackend(t, remote.ServerConfig{
		Name:    "relay",
		Kinds:   []backend.Kind{backend.KindPR},
		Threads: &capturingThreads{},
	}))

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"backends", "--json"})
	require.NoError(t, root.Execute())

	var infos []backend.Info
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &infos))
	byName := map[string]backend.Info{}
	for _, i := range infos {
		byName[i.Name] = i
	}

	assert.Equal(t, []backend.Capability{backend.CapThreads}, byName["relay"].Capabilities)
	// The built-in backend provides every capability, so it is always the
	// fallback for whatever an external backend leaves alone.
	assert.Contains(t, byName["gh"].Capabilities, backend.CapReview)
	assert.Contains(t, byName["gh"].Capabilities, backend.CapDraft)
	assert.Contains(t, byName["gh"].Capabilities, backend.CapReactions)
}
