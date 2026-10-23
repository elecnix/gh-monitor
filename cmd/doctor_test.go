package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/handoff"
)

// fakeInfo stands in for a daemon's report.
func fakeInfo(pid int, clients ...handoff.ClientInfo) func() {
	origInfo := handoffInfoFn
	handoffInfoFn = func(context.Context, string) (handoff.DaemonInfo, error) {
		return handoff.DaemonInfo{PID: pid, Socket: "/tmp/x.sock", StartedAt: time.Now(), Clients: clients}, nil
	}
	return func() { handoffInfoFn = origInfo }
}

func runDoctor(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(append([]string{"doctor"}, args...))
	err := root.Execute()
	return out.String() + errOut.String(), err
}

// TestDoctor_FlagsWatcherOutsideTheDaemon is the check that would have caught
// the fleet of pollers that spent the GraphQL budget: a watcher the daemon is
// not serving is a polling process nothing else can see (issue #193).
func TestDoctor_FlagsWatcherOutsideTheDaemon(t *testing.T) {
	restoreInfo := fakeInfo(100)
	defer restoreInfo()
	origScan := scanWatchers
	defer func() { scanWatchers = origScan }()
	stopped := 0
	scanWatchers = func() ([]watcherProcess, error) {
		return []watcherProcess{
			{PID: 200, Age: time.Hour, Describe: "-R elecnix/gh-monitor 192", Stop: func() error { stopped++; return nil }},
			{PID: 201, Age: time.Hour, Describe: "-R elecnix/gh-monitor 193", Stop: func() error { stopped++; return nil }},
		}, nil
	}

	out, err := runDoctor(t)
	require.Error(t, err, "a watcher polling outside the daemon must fail the command")
	assert.Contains(t, out, "NOT ATTACHED")
	assert.Contains(t, out, "pid 200")
	assert.Contains(t, out, "pid 201")
	assert.Zero(t, stopped, "a report must never stop a process")
}

func TestDoctor_AttachedFleetIsOK(t *testing.T) {
	restoreInfo := fakeInfo(100, handoff.ClientInfo{
		PID: 200, Watch: true, Since: time.Now(),
		Targets: []backend.Target{{Owner: "o", Repo: "r", Number: 7}},
	})
	defer restoreInfo()
	origScan := scanWatchers
	defer func() { scanWatchers = origScan }()
	scanWatchers = func() ([]watcherProcess, error) {
		return []watcherProcess{{PID: 200, Age: time.Hour, Describe: "-R o/r 7"}}, nil
	}

	out, err := runDoctor(t)
	require.NoError(t, err)
	assert.Contains(t, out, "o/r#7")
	assert.Contains(t, out, "every watcher is served by the daemon")
}

// TestDoctor_JustStartedWatcherIsNotAnOrphan: a client that has not attached
// yet is not a leak, and reporting it as one would make the command cry wolf
// on every concurrent launch.
func TestDoctor_JustStartedWatcherIsNotAnOrphan(t *testing.T) {
	restoreInfo := fakeInfo(100)
	defer restoreInfo()
	origScan := scanWatchers
	defer func() { scanWatchers = origScan }()
	scanWatchers = func() ([]watcherProcess, error) {
		return []watcherProcess{{PID: 200, Age: 2 * time.Second, Describe: "-R o/r 7"}}, nil
	}

	out, err := runDoctor(t)
	require.NoError(t, err)
	assert.Contains(t, out, "just started")
}

func TestDoctor_ReapStopsOnlyOldUnattachedWatchers(t *testing.T) {
	restoreInfo := fakeInfo(100)
	defer restoreInfo()
	origScan := scanWatchers
	defer func() { scanWatchers = origScan }()
	var reaped []int
	scanWatchers = func() ([]watcherProcess, error) {
		return []watcherProcess{
			{PID: 200, Age: time.Hour, Describe: "old", Stop: func() error { reaped = append(reaped, 200); return nil }},
			{PID: 201, Age: 30 * time.Second, Describe: "barely started", Stop: func() error { reaped = append(reaped, 201); return nil }},
		}, nil
	}

	out, err := runDoctor(t, "--reap")
	require.NoError(t, err, "reaping what is old enough is success; the young watcher is not an orphan yet")
	assert.Equal(t, []int{200}, reaped, "only the watcher older than --min-age may be stopped")
	assert.Contains(t, out, "reaped 1 of 1")
}

func TestDoctor_ReapReportsAFailedStop(t *testing.T) {
	restoreInfo := fakeInfo(100)
	defer restoreInfo()
	origScan := scanWatchers
	defer func() { scanWatchers = origScan }()
	scanWatchers = func() ([]watcherProcess, error) {
		return []watcherProcess{{PID: 200, Age: time.Hour, Describe: "old", Stop: func() error { return errors.New("no such process") }}}, nil
	}

	out, err := runDoctor(t, "--reap")
	require.Error(t, err)
	assert.Contains(t, out, "no such process")
}

// TestDoctor_NoWarningWhenTheDaemonSaidNothing is the logic inversion cite
// found: with no answer, Info is its zero value, so "not supervised" was being
// read out of silence rather than out of a report.
func TestDoctor_NoWarningWhenTheDaemonSaidNothing(t *testing.T) {
	origInfo := handoffInfoFn
	handoffInfoFn = func(context.Context, string) (handoff.DaemonInfo, error) {
		return handoff.DaemonInfo{}, errors.New("no daemon answered")
	}
	t.Cleanup(func() { handoffInfoFn = origInfo })

	origScan := scanWatchers
	scanWatchers = func() ([]watcherProcess, error) { return nil, nil }
	t.Cleanup(func() { scanWatchers = origScan })

	t.Setenv("NOTIFY_SOCKET", "/run/notify/1")
	out, _ := runDoctor(t)
	assert.NotContains(t, out, "reports no service manager",
		"a daemon that did not answer has reported nothing, least of all its supervision")
}

// TestDoctor_NeverReapsWithoutTheDaemonsClientList is the other half of the
// same mistake: an unanswering daemon makes every watcher look unattached, so
// --reap must refuse rather than stop a healthy fleet.
func TestDoctor_NeverReapsWithoutTheDaemonsClientList(t *testing.T) {
	origInfo := handoffInfoFn
	handoffInfoFn = func(context.Context, string) (handoff.DaemonInfo, error) {
		return handoff.DaemonInfo{}, errors.New("listening but did not answer")
	}
	t.Cleanup(func() { handoffInfoFn = origInfo })

	origScan := scanWatchers
	t.Cleanup(func() { scanWatchers = origScan })
	stopped := 0
	scanWatchers = func() ([]watcherProcess, error) {
		return []watcherProcess{{
			PID: 200, Age: time.Hour, Describe: "-R o/r 7",
			Stop: func() error { stopped++; return nil },
		}}, nil
	}

	out, err := runDoctor(t, "--reap")
	require.Error(t, err)
	assert.Zero(t, stopped, "without the daemon's client list, nothing may be stopped")
	assert.Contains(t, out, "did not report its clients")
}
