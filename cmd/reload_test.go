package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReload_NoDaemonReportsAndDoesNotSpawn verifies that reloading with no
// resident daemon is a friendly no-op: nothing is spawned, and the operator
// is told the settings simply apply at next start.
func TestReload_NoDaemonReportsAndDoesNotSpawn(t *testing.T) {
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-reload-absent-*.d"))

	var spawned int64
	orig := spawnDaemonFn
	spawnDaemonFn = func(string, time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	root := newRootCommand()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs([]string{"reload"})
	require.NoError(t, root.Execute())

	assert.Equal(t, int64(0), atomic.LoadInt64(&spawned),
		"reload must not spawn a daemon when none is running")
	assert.Contains(t, out.String()+errOut.String(), "no resident daemon",
		"the operator must be told there was nothing to reload")
}

// TestReload_SpawnsSuccessorWhenDaemonLive verifies the happy path: with a
// live daemon owning the socket, reload spawns a successor bound to the same
// socket. The successor's in-memory handoff adoption (issue #73) is what
// makes this a state-preserving reload rather than a cold restart; here we
// pin the client-side wiring (probe, spawn target, readiness wait).
func TestReload_SpawnsSuccessorWhenDaemonLive(t *testing.T) {
	sock := shortSocket(t, "ghmon-reload-live-*.d")
	t.Setenv("GH_MONITOR_SOCK", sock)

	// A live listener stands in for the resident daemon: reload's probe must
	// see it, and its readiness wait must succeed against it once the fake
	// spawn "completes".
	l, err := ipc.Listen(sock)
	require.NoError(t, err)
	go func() {
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close() }) //nolint:wsl — listener close after accept goroutine dies

	var spawned int64
	var spawnedAt string
	orig := spawnDaemonFn
	spawnDaemonFn = func(socket string, _ time.Duration) error {
		atomic.AddInt64(&spawned, 1)
		spawnedAt = socket
		return nil
	}
	t.Cleanup(func() { spawnDaemonFn = orig })

	root := newRootCommand()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs([]string{"reload"})
	require.NoError(t, root.Execute())

	assert.Equal(t, int64(1), atomic.LoadInt64(&spawned),
		"reload must spawn exactly one successor daemon")
	assert.Equal(t, sock, spawnedAt,
		"the successor must target the resident daemon's socket")
	assert.Contains(t, out.String()+errOut.String(), "carried over",
		"the operator must be told watching state survives the reload")
}

// TestPrefsSetWarnsOnDaemonReadKeys pins the prefs-set ergonomics loop
// (follow-up to issue #90): setting a key only the resident daemon reads
// prints a pointer at `gh monitor reload`, so the change is not silently
// pending until some future restart.
func TestPrefsSetWarnsOnDaemonReadKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCommand()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs([]string{"prefs", "set", `{"pollInterval": "10m", "idlePollCeiling": "6h"}`})
	require.NoError(t, root.Execute())

	combined := out.String() + errOut.String()
	assert.Contains(t, combined, "pollInterval")
	assert.Contains(t, combined, "idlePollCeiling")
	assert.Contains(t, combined, "gh monitor reload",
		"the warning must name the command that applies the settings now")
}

// Client-side keys (templates, ignoredBots, …) need no daemon involvement:
// no warning may fire for them.
func TestPrefsSetNoWarnForClientSideKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCommand()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs([]string{"prefs", "set", `{"ignoredBots": ["dependabot"]}`})
	require.NoError(t, root.Execute())

	assert.NotContains(t, out.String()+errOut.String(), "reload",
		"a client-side-only set must not suggest a daemon reload")
}

// TestReload_RefusesToClaimSuccessWithoutAnIdentifiableSuccessor pins the
// hardening a review asked for: "a pid different from the resident one" is
// satisfied by the resident daemon itself when its own report carried no pid,
// so reload must not accept that as a takeover.
func TestReload_RefusesToClaimSuccessWithoutAnIdentifiableSuccessor(t *testing.T) {
	sock := shortSocket(t, "ghmon-reload-unknowable-*.d")
	t.Setenv("GH_MONITOR_SOCK", sock)

	// A daemon that answers Info with no pid and "starts" a successor it does
	// not name: the two identities are indistinguishable.
	origInfo := handoffInfoFn
	t.Cleanup(func() { handoffInfoFn = origInfo })

	_, err := waitForNewDaemon(t.Context(), sock, 0, 0, 100*time.Millisecond)
	require.Error(t, err, "with neither pid known, a takeover cannot be verified")
	assert.Contains(t, err.Error(), "neither pid is known")
}

// TestWaitForNewDaemonWantsTheReportedPID covers the ordinary path: the daemon
// named the successor, and only that pid counts as serving.
func TestWaitForNewDaemonWantsTheReportedPID(t *testing.T) {
	sock := shortSocket(t, "ghmon-wait-new-*.d")

	l, err := ipc.Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	// Stand in for the successor: answer Info with the pid the daemon named.
	go func() {
		ctx := context.Background()
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				cfg := remote.ServerConfig{
					Name: DaemonBackendName,
					Source: backend.SourceFunc(func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
						return nil, errors.New("unused")
					}),
					HandleOp: func(_ context.Context, conn io.ReadWriter, req remote.Request) (bool, error) {
						if req.Op != handoff.OpInfo {
							return false, nil
						}
						raw, _ := json.Marshal(handoff.DaemonInfo{PID: 4242, Socket: sock})
						return true, remote.WriteFrame(conn, remote.Frame{Result: raw})
					},
				}
				_ = remote.Serve(ctx, conn, cfg)
			}(c)
		}
	}()

	got, err := waitForNewDaemon(t.Context(), sock, 111, 4242, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 4242, got)

	// A different pid than the one the daemon named is not the successor.
	_, err = waitForNewDaemon(t.Context(), sock, 111, 9999, 200*time.Millisecond)
	require.Error(t, err, "only the pid the daemon reported may count as the successor")
}
