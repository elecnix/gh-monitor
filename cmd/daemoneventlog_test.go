package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/eventlog"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/mux"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endingSubdaemon answers a watch with one first-poll and then ends the
// stream, which is the failover case: the watch is not done, so the router
// must move it to the hub.
type endingSubdaemon struct{}

func (s *endingSubdaemon) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	ch := make(chan backend.Update, 1)
	ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()}
	close(ch)
	return ch, nil
}

// logRecord is one decoded line of the event log.
type logRecord map[string]any

// readLogRecords decodes every line of the event log in dir into a map per
// line. The log is JSONL: one JSON object per line.
func readLogRecords(t *testing.T, dir string) []logRecord {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "the event log directory must exist")
	var out []logRecord
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			var rec logRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec),
				"each logged line must be valid JSON: %q", line)
			out = append(out, rec)
		}
		require.NoError(t, sc.Err())
		require.NoError(t, f.Close())
	}
	return out
}

// recordsOfKind returns the records whose "kind" field is want.
func recordsOfKind(records []logRecord, want string) []logRecord {
	var out []logRecord
	for _, r := range records {
		if r["kind"] == want {
			out = append(out, r)
		}
	}
	return out
}

// TestDaemon_EventLogRecordsHubFetchesAndFailovers is the end-to-end
// acceptance test for issue #122: the daemon's event log records the polling
// itself, not only the updates that were delivered.
//
// A log built only from delivered updates cannot show how often the hub
// polls, or which watches fell back to it after a sub-daemon failover — and
// a problem on the sub-daemon-served path is exactly what an operator goes
// to the log to diagnose.
func TestDaemon_EventLogRecordsHubFetchesAndFailovers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GH_MONITOR_AUTOSTART", "0")
	t.Setenv("GH_HOST", "")

	logDir := filepath.Join(dir, "events")
	cfgDir := filepath.Join(dir, "gh-monitor")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "preferences.json"),
		[]byte(`{"eventLog":{"dir":"`+logDir+`","keepDays":5}}`), 0o644))

	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// A sub-daemon that serves the PR kind and then ends its stream early:
	// the watch falls back to the hub, which must be recorded.
	sub := &endingSubdaemon{}
	brokerSock := shortSocket(t, "ghmon-log-broker-*.d")
	startFakeSubdaemon(t, serveCtx, brokerSock, []backend.Kind{backend.KindPR}, sub)

	var fetches int64
	h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
		fetches++
		return backlogPR(), nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	reg := mux.NewRegistry(io.Discard)
	tr, err := remote.ParseEndpoint("unix:" + brokerSock)
	require.NoError(t, err)
	reg.Track("broker", tr)
	reg.Probe(serveCtx)
	require.NotNil(t, reg.Provider(backend.KindPR))

	sock := shortSocket(t, "ghmon-log-*.d")
	l, err := ipc.Listen(sock)
	require.NoError(t, err)
	srv := bindTestServerOnWithRoutes(t, serveCtx, h, l, reg)

	// The daemon's event log is opened the way runDaemon opens it: from the
	// operator's preferences.
	daemonPrefs, err := prefs.Load("")
	require.NoError(t, err)
	evlog := openDaemonEventLog(newRootCommand(), daemonPrefs)
	require.NotNil(t, evlog, "the operator turned the event log on")
	t.Cleanup(func() { _ = evlog.Close() })
	h.SetFetchObserver(daemonFetchObserver(evlog))
	srv.evlog = evlog

	// The watch goes to the sub-daemon, which ends its stream, so the watch
	// fails over to the hub.
	ch, err := mux.RoutingSource{
		Reg: reg, Fallback: hubSource{hub: h},
		OnFailover: daemonFailoverObserver(evlog),
	}.Watch(serveCtx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
		backend.WatchOptions{ResumeID: "abc"})
	require.NoError(t, err)

	// Drain the watch until the failover notice arrives: that is the point
	// at which the watch moved to the hub, and the moment the failover is
	// recorded.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("the watch ended before the failover notice arrived")
			}
			if u.Event.Type == backend.EventDegraded && strings.Contains(u.Event.Notice, "fakebroker") {
				goto done
			}
		case <-deadline:
			t.Fatal("no failover notice")
		}
	}
done:
	// The hub's backlog fetch must have been observed.
	require.Positive(t, fetches, "the hub served the backlog")

	records := readLogRecords(t, logDir)

	// (2) Every hub fetch is recorded with its target, tier, and whether it
	// changed anything.
	fetchRecords := recordsOfKind(records, eventlog.KindHubFetch)
	require.NotEmpty(t, fetchRecords, "the hub fetch must be recorded")
	fr := fetchRecords[0]
	assert.Equal(t, "pr", fr["target"].(map[string]any)["kind"], "the fetch names the target's kind")
	assert.Equal(t, "o", fr["target"].(map[string]any)["owner"], "the fetch names the target's owner")
	assert.Equal(t, "r", fr["target"].(map[string]any)["repo"], "the fetch names the target's repo")
	assert.Equal(t, float64(7), fr["target"].(map[string]any)["number"], "the fetch names the target's number")
	assert.NotEmpty(t, fr["tier"], "the fetch names the tier it ran at")
	assert.Contains(t, fr, "changed", "the fetch says whether it changed anything")
	assert.NotEmpty(t, fr["at"], "the fetch says when it happened")

	// (3) The failover from the sub-daemon to the hub is recorded.
	failoverRecords := recordsOfKind(records, eventlog.KindHubFailover)
	require.NotEmpty(t, failoverRecords, "the failover must be recorded")
	fv := failoverRecords[0]
	assert.Equal(t, "fakebroker", fv["from"], "the failover names the sub-daemon that stopped serving")
	assert.Equal(t, "o", fv["target"].(map[string]any)["owner"], "the failover names the watch that moved")
	assert.NotEmpty(t, fv["reason"], "the failover says why the watch moved")
	assert.NotEmpty(t, fv["at"], "the failover says when it happened")
}
