package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// bindOutcomeHarness runs a monitor whose autostarted daemon reports the
// given bind outcome, and returns the command's stderr.
//
// The stub stands in for the daemon a client spawns. It binds the socket — so
// the watch has a server to talk to, exactly as it does when a concurrent
// client wins the race — and then writes the outcome the real daemon writes
// after its own ipc.Listen call. outcome is what the daemon reports: ipc.BindWon,
// ipc.BindLost, or "" for a daemon that reports nothing at all.
//
// The client's own liveness probe still finds no daemon, because the socket
// is only bound once the spawn happens: that is what makes this the cold-start
// race rather than the ordinary already-running case.
func bindOutcomeHarness(t *testing.T, outcome string, args ...string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	t.Setenv("GH_MONITOR_AUTOSTART", "1")
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-bind-*.d"))

	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return backlogPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	origSpawn := spawnDaemonFn
	t.Cleanup(func() { spawnDaemonFn = origSpawn })
	spawnDaemonFn = func(socket string, _ time.Duration, bindResultPath string) error {
		// The winner binds the socket. A client whose probe found nothing
		// will connect to it, which is the whole point of the race.
		bindTestServer(t, serveCtx, h, socket)
		// Then the daemon reports what its own bind attempt decided.
		if outcome != "" {
			ipc.WriteBindResultTo(bindResultPath, outcome)
		}
		return nil
	}

	origFactory := apiClientFactory
	t.Cleanup(func() { apiClientFactory = origFactory })
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(append([]string{"7", "-R", "o/r", "--timeout", "1"}, args...))
	// The notice assertions below are mostly NotContains, so a command that
	// died before reaching the notice path would leave stderr empty and pass
	// them for the wrong reason. Fail loudly instead.
	if err := root.Execute(); err != nil {
		t.Fatalf("monitor: %v\nstderr: %s", err, stderr.String())
	}
	return stderr.String()
}

// TestMonitor_IntervalNoticeSurvivesLostBindRace is the regression test for
// issue #170: the concurrent cold start must not suppress the notice.
//
// Two clients cold-start the daemon at once. The winner binds the socket;
// the loser's spawned daemon loses the bind, and the loser must therefore
// learn that the daemon serving its watch is somebody else's — so the
// --interval it asked for never reached the poller.
//
// Before the fix the loser concluded it had started the daemon (WaitReady
// succeeds against the winner), suppressed the notice, and left the operator
// believing they got a cadence they did not get.
func TestMonitor_IntervalNoticeSurvivesLostBindRace(t *testing.T) {
	stderr := bindOutcomeHarness(t, ipc.BindLost, "--interval", "42")
	assert.Contains(t, stderr, "--interval 42s",
		"a daemon that lost the bind race serves the watch, so the operator must learn their --interval was dropped")
	assert.Contains(t, stderr, "pollInterval",
		"the notice must name the preference that actually sets the cadence")
}

// TestMonitor_IntervalSilentWhenOwnDaemonWonBind is the other half: when this
// invocation's own daemon won the socket, it runs at the cadence this
// invocation asked for, and there is nothing to report.
func TestMonitor_IntervalSilentWhenOwnDaemonWonBind(t *testing.T) {
	stderr := bindOutcomeHarness(t, ipc.BindWon, "--interval", "42")
	assert.NotContains(t, stderr, "--interval 42s",
		"the daemon this invocation started was given the cadence; got %q", stderr)
}

// TestMonitor_IntervalSilentWhenBindOutcomeUnknown covers a daemon that
// reports nothing: an older build, or one that died before it could write the
// file. The answer is unknown, so the client falls back to its previous
// behaviour — assume the daemon it spawned is the one serving — and the watch
// proceeds. An unknown answer must never break a watch.
func TestMonitor_IntervalSilentWhenBindOutcomeUnknown(t *testing.T) {
	stderr := bindOutcomeHarness(t, "", "--interval", "42")
	assert.NotContains(t, stderr, "--interval 42s",
		"an unknown bind outcome falls back to the previous behaviour; got %q", stderr)
}

// TestMonitor_IntervalNoticeWhenDaemonAlreadyListening is the ordinary case,
// kept explicit: a daemon that was already listening when the invocation
// started never received this client's --interval, so the notice fires.
func TestMonitor_IntervalNoticeWhenDaemonAlreadyListening(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	t.Setenv("GH_MONITOR_AUTOSTART", "1")
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-already-*.d"))

	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return backlogPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)
	bindTestServer(t, serveCtx, h, shortSocket(t, "unused-*.d"))

	// Bind the daemon on the socket the client will use, before the client
	// starts: the daemon is already there when the client arrives.
	sock := shortSocket(t, "ghmon-already-sock-*.d")
	bindTestServer(t, serveCtx, h, sock)
	t.Setenv("GH_MONITOR_SOCK", sock)

	// The client must not spawn anything: a daemon is already serving.
	var spawned int
	origSpawn := spawnDaemonFn
	t.Cleanup(func() { spawnDaemonFn = origSpawn })
	spawnDaemonFn = func(string, time.Duration, string) error {
		spawned++
		return nil
	}

	origFactory := apiClientFactory
	t.Cleanup(func() { apiClientFactory = origFactory })
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"7", "-R", "o/r", "--timeout", "1", "--interval", "42"})
	require.NoError(t, root.Execute())

	assert.Equal(t, 0, spawned, "a daemon was already listening; nothing should be spawned")
	assert.Contains(t, stderr.String(), "--interval 42s",
		"the running daemon keeps its own cadence, so the operator must be told")
}

// TestAutoStart_ReportsBindOutcome checks the fact attachDaemon is built on:
// autostartDaemon reports whether THIS invocation's daemon won the socket.
// The daemon that reports "lost" is serving nothing, so the caller must be
// able to tell the two apart.
func TestAutoStart_ReportsBindOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		want    bool
	}{
		{"won", ipc.BindWon, true},
		{"lost", ipc.BindLost, false},
		{"unknown", "", true}, // no report at all: fall back to the old behaviour
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
				return backlogPR(), nil
			}, nil, time.Hour, nil)
			t.Cleanup(h.Stop)

			t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-outcome-*.d"))

			orig := spawnDaemonFn
			t.Cleanup(func() { spawnDaemonFn = orig })
			spawnDaemonFn = func(socket string, _ time.Duration, bindResultPath string) error {
				bindTestServer(t, ctx, h, socket)
				if tc.outcome != "" {
					ipc.WriteBindResultTo(bindResultPath, tc.outcome)
				}
				return nil
			}

			started, err := autostartDaemon(ctx, daemonSocketPath(), time.Minute)
			require.NoError(t, err)
			assert.Equal(t, tc.want, started,
				"autostartDaemon must report whether this invocation's daemon won the socket")
		})
	}
}
