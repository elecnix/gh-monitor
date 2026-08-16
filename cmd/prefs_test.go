package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runPrefs builds the prefs command, points --config-dir at a temp dir, sets
// args, executes, and returns stdout.
func runPrefs(t *testing.T, configDir string, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := newPrefsCommand()
	cmd.SetArgs(append([]string{"--config-dir", configDir}, args...))
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&bytes.Buffer{})
	err = cmd.Execute()
	return buf.String(), err
}

func prefsFile(t *testing.T, configDir string) string {
	t.Helper()
	return filepath.Join(configDir, "gh-monitor", "preferences.json")
}

func TestPrefsGetPrintsDefaults(t *testing.T) {
	dir := t.TempDir()
	out, err := runPrefs(t, dir, "get")
	require.NoError(t, err)
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	require.Contains(t, p, "templates")
	tmpl, ok := p["templates"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, tmpl, "conflict")
	assert.Equal(t, false, p["retriggerComments"])
}

func TestPrefsBareAliasForGet(t *testing.T) {
	dir := t.TempDir()
	out, err := runPrefs(t, dir)
	require.NoError(t, err)
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	assert.Contains(t, p, "templates")
}

func TestPrefsSetAppliesOverride(t *testing.T) {
	dir := t.TempDir()
	out, err := runPrefs(t, dir, "set", `{"templates":{"conflict":"⚠️ {prLabel}"}}`)
	require.NoError(t, err)
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	tmpl := p["templates"].(map[string]interface{})
	assert.Equal(t, "⚠️ {prLabel}", tmpl["conflict"])

	// File should exist and persist only the override.
	data, err := os.ReadFile(prefsFile(t, dir))
	require.NoError(t, err)
	var stored map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &stored))
	storedTmpl := stored["templates"].(map[string]interface{})
	assert.Len(t, storedTmpl, 1)
	assert.Contains(t, storedTmpl, "conflict")
}

func TestPrefsSetFileFlag(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "overrides.json")
	require.NoError(t, os.WriteFile(src, []byte(`{"ignoredBots":["bot1"]}`), 0o644))
	out, err := runPrefs(t, dir, "set", "--file", src)
	require.NoError(t, err)
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	assert.Equal(t, []interface{}{"bot1"}, p["ignoredBots"])
}

func TestPrefsSetStdin(t *testing.T) {
	dir := t.TempDir()
	orig := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString(`{"retriggerComments":true}`)
	require.NoError(t, err)
	_ = w.Close()
	os.Stdin = r
	defer func() { os.Stdin = orig }()

	out, err := runPrefs(t, dir, "set", "--file", "-")
	require.NoError(t, err)
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	assert.Equal(t, true, p["retriggerComments"])
}

func TestPrefsSetRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	_, err := runPrefs(t, dir, "set", `{"bogus":1}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown preference key")
}

func TestPrefsSetRequiresInput(t *testing.T) {
	dir := t.TempDir()
	_, err := runPrefs(t, dir, "set")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provide a JSON argument")
}

func TestPrefsResetClearsFile(t *testing.T) {
	dir := t.TempDir()
	_, err := runPrefs(t, dir, "set", `{"templates":{"conflict":"x {prLabel}"}}`)
	require.NoError(t, err)
	require.FileExists(t, prefsFile(t, dir))

	out, err := runPrefs(t, dir, "reset")
	require.NoError(t, err)
	_, statErr := os.Stat(prefsFile(t, dir))
	assert.True(t, os.IsNotExist(statErr))

	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	tmpl := p["templates"].(map[string]interface{})
	// Back to default.
	assert.Contains(t, tmpl, "conflict")
}

func TestPrefsPathPrintsConfigPath(t *testing.T) {
	dir := t.TempDir()
	out, err := runPrefs(t, dir, "path")
	require.NoError(t, err)
	assert.Equal(t, prefsFile(t, dir)+"\n", out)
}

func TestPrefsGetReflectsFileOverrides(t *testing.T) {
	dir := t.TempDir()
	_, err := runPrefs(t, dir, "set", `{"templates":{"merged":"M {prLabel}"}}`)
	require.NoError(t, err)
	out, err := runPrefs(t, dir, "get")
	require.NoError(t, err)
	var p map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	tmpl := p["templates"].(map[string]interface{})
	assert.Equal(t, "M {prLabel}", tmpl["merged"])
}
