package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonitor_EventLogWritesJSONL verifies the event log wiring end to end
// (issue #86): with eventLog turned on in the global preferences, a watch
// writes every delivered backend update — above the backend layer, before
// rendering — as JSONL into the configured directory.
func TestMonitor_EventLogWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GH_HOST", "")
	logDir := filepath.Join(dir, "events")
	cfgDir := filepath.Join(dir, "gh-monitor")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "preferences.json"),
		[]byte(`{"eventLog":{"dir":"`+logDir+`","keepDays":5}}`), 0o644))

	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	entries, err := os.ReadDir(logDir)
	require.NoError(t, err, "the event log directory must exist once eventLog is on")
	require.Len(t, entries, 1, "one daily file")
	name := entries[0].Name()
	require.True(t, strings.HasPrefix(name, "events-") && strings.HasSuffix(name, ".jsonl"),
		"daily file naming: events-YYYY-MM-DD.jsonl, got %q", name)

	f, err := os.Open(filepath.Join(logDir, name))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var sawFirstPoll, sawFailing bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		require.True(t, json.Valid([]byte(line)), "each logged line must be valid JSON: %q", line)
		var u map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &u))
		if u["event"] != nil {
			ev := u["event"].(map[string]any)
			if ev["type"] == "first-poll" {
				sawFirstPoll = true
			}
			if ev["type"] == "new-failing-checks" {
				sawFailing = true
			}
		}
	}
	require.NoError(t, sc.Err())
	assert.True(t, sawFirstPoll, "the first-poll update must be logged")
	assert.True(t, sawFailing, "the failing-check update must be logged")
}

// TestMonitor_EventLogOffByDefault verifies no log directory appears when the
// operator has not turned the event log on — it is strictly opt-in.
func TestMonitor_EventLogOffByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GH_HOST", "")

	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--once"})
	require.NoError(t, root.Execute())

	_, statErr := os.Stat(filepath.Join(dir, "events"))
	assert.True(t, os.IsNotExist(statErr), "no event log directory without eventLog config")

	// The default location must not appear either.
	def := DefaultEventLogDir()
	if def != filepath.Join(dir, "events") { // only meaningful when XDG points at the temp dir
		_, statErr = os.Stat(def)
		assert.True(t, os.IsNotExist(statErr) || statErr != nil, "default event log dir must not be created when off")
	}
}
