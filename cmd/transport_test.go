package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/internal/broker"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/transportlog"
)

type fakeBroadcaster struct{ notices []string }

func (f *fakeBroadcaster) BroadcastNotice(s string) { f.notices = append(f.notices, s) }

var transportT0 = time.Date(2026, 11, 10, 17, 4, 57, 0, time.UTC)

// TestTransportObserver_LogsEveryTransitionAndNoticesEpisodes: every
// transition is one log record, and only the outage and the recovery
// become notices. A degraded transition is already a notice through the
// hub's health path, so it must not become a second one here.
func TestTransportObserver_LogsEveryTransitionAndNoticesEpisodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport.jsonl")
	w, err := transportlog.Open(path, transportlog.DefaultMaxBytes)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	b := &fakeBroadcaster{}
	stderr := &bytes.Buffer{}
	observe := transportObserver(b, w, stderr)

	since := transportT0
	observe(broker.Transition{Kind: broker.TransitionDegraded, At: since, Host: "relay.example.com", Attempt: 1, Since: since,
		Err: errors.New("connection lost: websocket: close 1006 (abnormal closure): unexpected EOF"), RetryIn: 2 * time.Second})
	observe(broker.Transition{Kind: broker.TransitionOutage, At: since.Add(2 * time.Minute), Host: "relay.example.com", Attempt: 7, Since: since})
	observe(broker.Transition{Kind: broker.TransitionRecovered, At: since.Add(5 * time.Minute), Host: "relay.example.com", Attempt: 9, Since: since})

	require.Len(t, b.notices, 2, "the degraded transition is not a notice here; the outage and the recovery are")
	assert.Contains(t, b.notices[0], "⚠️ broker transport down for 2m0s after 7 failed reconnect attempts")
	assert.Contains(t, b.notices[0], "since 2026-11-10T17:04:57Z")
	assert.Contains(t, b.notices[0], "falling back to polling")
	assert.Contains(t, b.notices[1], "✅ broker transport recovered after 5m0s down (9 attempts")
	assert.Contains(t, b.notices[1], "observed by polling only")

	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var events []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r transportlog.Record
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		events = append(events, r.Event)
	}
	assert.Equal(t, []string{"degraded", "outage", "recovered"}, events)
	assert.Empty(t, stderr.String())
}

// TestTransportObserver_SurvivesWithoutALog: a daemon whose log could not be
// opened still tells its clients.
func TestTransportObserver_SurvivesWithoutALog(t *testing.T) {
	b := &fakeBroadcaster{}
	observe := transportObserver(b, nil, &bytes.Buffer{})
	observe(broker.Transition{Kind: broker.TransitionOutage, At: transportT0.Add(time.Minute), Attempt: 3, Since: transportT0})
	require.Len(t, b.notices, 1)
}

func TestTransportLogPath(t *testing.T) {
	t.Setenv("GH_MONITOR_TRANSPORT_LOG", "/tmp/elsewhere/transport.jsonl")
	assert.Equal(t, "/tmp/elsewhere/transport.jsonl", transportLogPath())
	t.Setenv("GH_MONITOR_TRANSPORT_LOG", "")
	assert.Equal(t, "transport.jsonl", filepath.Base(transportLogPath()))
	assert.Equal(t, "gh-monitor", filepath.Base(filepath.Dir(transportLogPath())))
}

// writeTransportLog writes transitions to a fresh log and points doctor at it.
func writeTransportLog(t *testing.T, trs ...broker.Transition) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transport.jsonl")
	w, err := transportlog.Open(path, transportlog.DefaultMaxBytes)
	require.NoError(t, err)
	for _, tr := range trs {
		tr.Host = "relay.example.com"
		require.NoError(t, w.Log(transportlog.FromTransition(tr)))
	}
	require.NoError(t, w.Close())
	t.Setenv("GH_MONITOR_TRANSPORT_LOG", path)
	return path
}

func noWatchers(t *testing.T) {
	t.Helper()
	origScan := scanWatchers
	scanWatchers = func() ([]watcherProcess, error) { return nil, nil }
	t.Cleanup(func() { scanWatchers = origScan })
}

// daemonStarted fakes a daemon that began serving at startedAt, with no
// clients. The transport's first record is written at the daemon's start,
// so a test about a daemon's own connection must date the daemon before it.
func daemonStarted(t *testing.T, startedAt time.Time) {
	t.Helper()
	origInfo := handoffInfoFn
	handoffInfoFn = func(context.Context, string) (handoff.DaemonInfo, error) {
		return handoff.DaemonInfo{PID: 100, Socket: "/tmp/x.sock", StartedAt: startedAt}, nil
	}
	t.Cleanup(func() { handoffInfoFn = origInfo })
}

// pinDoctorClock fixes the clock doctor computes ages from, so an age like
// "down for 9m0s" does not depend on how long the test took to get there.
func pinDoctorClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := doctorNow
	doctorNow = func() time.Time { return now }
	t.Cleanup(func() { doctorNow = orig })
}

// TestDoctor_ReportsHealthyTransport: with the transport connected, doctor
// says so, with the host and how long it has been up.
func TestDoctor_ReportsHealthyTransport(t *testing.T) {
	now := time.Now()
	pinDoctorClock(t, now)
	daemonStarted(t, now.Add(-10*time.Minute))
	noWatchers(t)
	writeTransportLog(t,
		broker.Transition{Kind: broker.TransitionConnecting, At: now.Add(-10 * time.Minute), Attempt: 1},
		broker.Transition{Kind: broker.TransitionHealthy, At: now.Add(-10 * time.Minute)},
	)

	out, err := runDoctor(t)
	require.NoError(t, err)
	assert.Contains(t, out, "broker transport: healthy for 10m0s (host relay.example.com)")
	assert.NotContains(t, out, "last error")
	assert.NotContains(t, out, "down for")
	assert.NotContains(t, out, "predates this daemon")
	assert.Contains(t, out, "every watcher is served by the daemon", "the existing report is unchanged")
}

// TestDoctor_ReportsFailingTransport: with the transport down, doctor says
// the state, the attempt, the delay, the last error's class, when it was last
// healthy, and that the watches were told.
func TestDoctor_ReportsFailingTransport(t *testing.T) {
	now := time.Now()
	pinDoctorClock(t, now)
	daemonStarted(t, now.Add(-time.Hour))
	noWatchers(t)
	down := now.Add(-9 * time.Minute)
	writeTransportLog(t,
		broker.Transition{Kind: broker.TransitionHealthy, At: now.Add(-time.Hour)},
		broker.Transition{Kind: broker.TransitionDegraded, At: down, Attempt: 1, Since: down, RetryIn: 2 * time.Second,
			Err: errors.New("connection lost: websocket: close 1006 (abnormal closure): unexpected EOF")},
		broker.Transition{Kind: broker.TransitionOutage, At: down.Add(2 * time.Minute), Attempt: 6, Since: down},
		broker.Transition{Kind: broker.TransitionConnecting, At: now.Add(-time.Minute), Attempt: 8},
		broker.Transition{Kind: broker.TransitionDegraded, At: now.Add(-30 * time.Second), Attempt: 8, Since: down, RetryIn: 60 * time.Second,
			Err: errors.New("connect: network Error : dial tcp: i/o timeout")},
	)

	out, err := runDoctor(t)
	require.NoError(t, err)
	assert.Contains(t, out, "broker transport: degraded for ")
	assert.Contains(t, out, "attempt 8")
	assert.Contains(t, out, "retrying in 1m0s")
	assert.Contains(t, out, "last healthy 1h0m0s ago")
	assert.Contains(t, out, "last error 30s ago (connect): connect: network Error : dial tcp: i/o timeout")
	assert.Contains(t, out, "down for 9m0s; the watches were told")
}

// TestDoctor_NoTransportLogSaysNothing: an install with no broker has no
// log, and doctor's report is the one it always printed.
func TestDoctor_NoTransportLogSaysNothing(t *testing.T) {
	restoreInfo := fakeInfo(100)
	defer restoreInfo()
	noWatchers(t)
	t.Setenv("GH_MONITOR_TRANSPORT_LOG", filepath.Join(t.TempDir(), "none.jsonl"))

	out, err := runDoctor(t)
	require.NoError(t, err)
	assert.NotContains(t, out, "broker transport")
}

// TestDoctor_StaleTransportLogIsLabelled: a log left by an earlier daemon
// must not read as the current daemon's connection.
func TestDoctor_StaleTransportLogIsLabelled(t *testing.T) {
	daemonStarted(t, time.Now())
	noWatchers(t)
	writeTransportLog(t, broker.Transition{Kind: broker.TransitionHealthy, At: time.Now().Add(-2 * time.Hour)})

	out, err := runDoctor(t)
	require.NoError(t, err)
	assert.Contains(t, out, "predates this daemon")
	assert.Contains(t, out, "broker transport: healthy for ")
}
