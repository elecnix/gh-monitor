package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// intervalHarness binds an in-process daemon on $GH_MONITOR_SOCK and runs a
// monitor against it, returning the command's stderr. A daemon already
// listening is the normal topology: one poller shared by every watcher, so the
// cadence comes from the hub's own construction-time interval, not from the
// client's --interval.
func intervalHarness(t *testing.T, autostart bool, args ...string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	if autostart {
		t.Setenv("GH_MONITOR_AUTOSTART", "1")
	} else {
		t.Setenv("GH_MONITOR_AUTOSTART", "0")
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		return backlogPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	sock := shortSocket(t, "ghmon-interval-*.d")
	bindTestServer(t, serveCtx, h, sock)
	t.Setenv("GH_MONITOR_SOCK", sock)

	origFactory := apiClientFactory
	t.Cleanup(func() { apiClientFactory = origFactory })
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(append([]string{"7", "-R", "o/r", "--timeout", "1"}, args...))
	// The silence assertions below are NotContains, so a command that died
	// before reaching the notice path would leave stderr empty and pass them
	// for the wrong reason. Fail loudly instead.
	if err := root.Execute(); err != nil {
		t.Fatalf("monitor: %v\nstderr: %s", err, stderr.String())
	}
	return stderr.String()
}

// TestMonitor_IntervalIgnoredByRunningDaemon is the regression test for the
// dead --interval flag. Every continuous watch is served by the shared-poller
// daemon, whose poller cadence comes from hub.New -- not from the client's
// WatchOptions.Interval, which hub.Subscribe never reads. An operator who
// passes --interval and is served by a daemon this invocation did not start
// does NOT get that cadence, and is told so rather than left to assume it.
func TestMonitor_IntervalIgnoredByRunningDaemon(t *testing.T) {
	stderr := intervalHarness(t, false, "--interval", "42")
	assert.Contains(t, stderr, "--interval 42s",
		"the operator must learn the cadence they asked for was not applied")
	assert.Contains(t, stderr, "pollInterval",
		"the notice must name the preference that actually sets the cadence")
	assert.Contains(t, stderr, "gh monitor daemon",
		"the notice must name the flag that actually sets the cadence")
}

// TestMonitor_IntervalSilentByDefault keeps the notice out of the way of a
// caller who never asked for a cadence. The default 300s is not a request.
func TestMonitor_IntervalSilentByDefault(t *testing.T) {
	stderr := intervalHarness(t, false)
	assert.NotContains(t, stderr, "--interval",
		"an unset --interval must not produce a notice; got %q", stderr)
}

// TestMonitor_IntervalSilentForOnceRead covers --once: a single fetch has no
// cadence at all, so there is nothing for the flag to be ignored by.
func TestMonitor_IntervalSilentForOnceRead(t *testing.T) {
	stderr := intervalHarness(t, false, "--interval", "42", "--once")
	assert.NotContains(t, stderr, "--interval",
		"a --once read never polls, so the notice is noise; got %q", stderr)
}

// TestMonitor_IntervalSilentWhenThisInvocationStartedTheDaemon closes the
// other half of the contract: when this process autostarts the daemon, it
// passes --interval down as the daemon's own --interval, so the request was
// honoured and there is nothing to report.
func TestMonitor_IntervalSilentWhenThisInvocationStartedTheDaemon(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")
	t.Setenv("GH_MONITOR_SOCK", shortSocket(t, "ghmon-interval-spawn-*.d"))
	t.Setenv("GH_MONITOR_AUTOSTART", "1")

	// A no-op daemon spawn that stands in for the re-exec: attachDaemon must
	// report that it started one, which is what makes the notice silent.
	origSpawn := spawnDaemonFn
	t.Cleanup(func() { spawnDaemonFn = origSpawn })
	spawnDaemonFn = func(socket string, interval time.Duration) error {
		serveCtx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
			return backlogPR(), nil
		}, nil, time.Hour, nil)
		t.Cleanup(h.Stop)
		bindTestServer(t, serveCtx, h, socket)
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
	_ = root.Execute()

	assert.NotContains(t, stderr.String(), "--interval 42s",
		"a daemon this invocation started was given the cadence; got %q", stderr.String())
}

// TestWriteIntervalIgnoredNotice checks the notice names the requested
// cadence in the operator's own units, so it is unambiguous which request went
// unapplied.
func TestWriteIntervalIgnoredNotice(t *testing.T) {
	var b bytes.Buffer
	writeIntervalIgnoredNotice(&b, 42*time.Second)
	out := b.String()
	require.NotEmpty(t, out)
	assert.Contains(t, out, "--interval 42s")
	assert.Contains(t, out, "pollInterval")
	assert.Contains(t, out, "gh monitor daemon --interval")
	assert.Equal(t, 1, strings.Count(out, "\n"),
		"the notice is one line, not a paragraph; got %q", out)
}
