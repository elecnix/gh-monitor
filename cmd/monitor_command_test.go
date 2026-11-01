package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/require"
)

// writeTestPrefs points the preferences at a temporary config dir holding
// prefsJSON.
//
// It is the setup every monitor-command test that reads a preference needs:
// the file goes where `gh monitor prefs set` would put it, so the test
// exercises the same load path an operator's own settings take.
func writeTestPrefs(t *testing.T, prefsJSON string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if prefsJSON == "" {
		return
	}
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gh-monitor")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "preferences.json"), []byte(prefsJSON), 0o644))
}

// runMonitorCommand runs the monitor command once against a fake GraphQL API
// that serves payload, and returns what it wrote to stdout and stderr.
//
// It is the plumbing every monitor-command test needs: point the API client
// at a fake, run the root command with the given extra flags, and collect
// the output. The fake's behaviour is the caller's business — a test that
// needs to watch the reactions installs its own client and drives the
// command itself.
func runMonitorCommand(t *testing.T, prefsJSON string, payload obj, extraArgs ...string) (stdout, stderr string) {
	t.Helper()
	writeTestPrefs(t, prefsJSON)
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	apiClientFactory = func(string) ghcli.API {
		return &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assignJSON(result, payload)
		}}
	}

	root := newRootCommand()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetArgs(append([]string{"7", "-R", "o/r", "--once"}, extraArgs...))
	require.NoError(t, root.Execute())
	return outBuf.String(), errBuf.String()
}
