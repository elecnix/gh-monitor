package prefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePrefsFile writes a stored-shape JSON document to the gh-monitor config
// dir under base, returning the file path.
func writePrefsFile(t *testing.T, base string, doc string) string {
	t.Helper()
	dir := filepath.Join(base, "gh-monitor")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, "preferences.json")
	require.NoError(t, os.WriteFile(p, []byte(doc), 0o644))
	return p
}

func readStoredFile(t *testing.T, base string) map[string]interface{} {
	t.Helper()
	p, err := ConfigPath(base)
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func TestUpdateFileSetsTemplateOverride(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{"templates":{"conflict":"⚠️ {prLabel} conflict!"}}`))
	require.NoError(t, err)
	assert.Equal(t, "⚠️ {prLabel} conflict!", eff.Templates["conflict"])
	// Other templates keep their defaults.
	assert.Equal(t, defaultTemplates["merged"], eff.Templates["merged"])

	stored := readStoredFile(t, base)
	require.Contains(t, stored, "templates")
	tmpl := stored["templates"].(map[string]interface{})
	// Only the overridden key is persisted; defaults are NOT written out.
	keys := make([]string, 0, len(tmpl))
	for k := range tmpl {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"conflict"}, keys)
	assert.Equal(t, "⚠️ {prLabel} conflict!", tmpl["conflict"])
}

func TestUpdateFileNullResetsTemplateToDefault(t *testing.T) {
	base := t.TempDir()
	writePrefsFile(t, base, `{"templates":{"conflict":"custom {prLabel}"}}`)
	eff, err := UpdateFile(base, []byte(`{"templates":{"conflict":null}}`))
	require.NoError(t, err)
	assert.Equal(t, defaultTemplates["conflict"], eff.Templates["conflict"])
	// The key is removed from the stored file (reset to default).
	stored := readStoredFile(t, base)
	tmpl, _ := stored["templates"].(map[string]interface{})
	_, present := tmpl["conflict"]
	assert.False(t, present, "reset key should be removed from file")
}

func TestUpdateFileMergesOverExistingFile(t *testing.T) {
	base := t.TempDir()
	writePrefsFile(t, base, `{"templates":{"conflict":"c {prLabel}"},"ignoredBots":["bot1"]}`)
	_, err := UpdateFile(base, []byte(`{"templates":{"merged":"m {prLabel}"}}`))
	require.NoError(t, err)
	eff, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, "c {prLabel}", eff.Templates["conflict"])
	assert.Equal(t, "m {prLabel}", eff.Templates["merged"])
	assert.Equal(t, []string{"bot1"}, eff.IgnoredBots)
}

func TestUpdateFileSetsIgnoredBotsReplacing(t *testing.T) {
	base := t.TempDir()
	writePrefsFile(t, base, `{"ignoredBots":["old"]}`)
	eff, err := UpdateFile(base, []byte(`{"ignoredBots":["a","b"]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, eff.IgnoredBots)
	// Empty array clears the list.
	eff, err = UpdateFile(base, []byte(`{"ignoredBots":[]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{}, eff.IgnoredBots)
}

func TestUpdateFileSetsRetriggerComments(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{"retriggerComments":true}`))
	require.NoError(t, err)
	assert.True(t, eff.RetriggerComments)
	// null resets to default (false) and removes from file.
	eff, err = UpdateFile(base, []byte(`{"retriggerComments":null}`))
	require.NoError(t, err)
	assert.False(t, eff.RetriggerComments)
	stored := readStoredFile(t, base)
	_, present := stored["retriggerComments"]
	assert.False(t, present, "null retriggerComments should be removed from file")
}

func TestUpdateFileRejectsUnknownTemplateKey(t *testing.T) {
	base := t.TempDir()
	_, err := UpdateFile(base, []byte(`{"templates":{"bogus":"{prLabel}"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown template key")
}

func TestUpdateFileRejectsTemplatelessValue(t *testing.T) {
	base := t.TempDir()
	_, err := UpdateFile(base, []byte(`{"templates":{"conflict":"no tokens here"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no {token}")
}

func TestUpdateFileRejectsUnknownTopLevelKey(t *testing.T) {
	base := t.TempDir()
	_, err := UpdateFile(base, []byte(`{"foo":"bar"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown preference key")
}

func TestUpdateFileEmptyObjectIsNoOp(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, DefaultPreferences(), eff)
	// No file should be created for an empty override document.
	p, err := ConfigPath(base)
	require.NoError(t, err)
	_, statErr := os.Stat(p)
	assert.True(t, os.IsNotExist(statErr), "no file should be written for empty overrides")
}

func TestUpdateFileInvalidJSON(t *testing.T) {
	base := t.TempDir()
	_, err := UpdateFile(base, []byte(`{not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse overrides")
}

func TestResetFileRemovesFileAndReturnsDefaults(t *testing.T) {
	base := t.TempDir()
	writePrefsFile(t, base, `{"templates":{"conflict":"x {prLabel}"}}`)
	eff, err := ResetFile(base)
	require.NoError(t, err)
	assert.Equal(t, DefaultPreferences(), eff)
	p, err := ConfigPath(base)
	require.NoError(t, err)
	_, statErr := os.Stat(p)
	assert.True(t, os.IsNotExist(statErr))
}

func TestResetFileNoOpWhenMissing(t *testing.T) {
	base := t.TempDir()
	eff, err := ResetFile(base)
	require.NoError(t, err)
	assert.Equal(t, DefaultPreferences(), eff)
}

func TestFilePathReturnsConfigPath(t *testing.T) {
	base := t.TempDir()
	p, err := FilePath(base)
	require.NoError(t, err)
	expected, err := ConfigPath(base)
	require.NoError(t, err)
	assert.Equal(t, expected, p)
}

func TestUpdateFileSetsSelfUpdate(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{"selfUpdate":"30m"}`))
	require.NoError(t, err)
	assert.Equal(t, "30m", eff.SelfUpdate)

	// Persisted to the file, visible to a fresh Load.
	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, "30m", got.SelfUpdate)

	// A second set replaces the value.
	eff, err = UpdateFile(base, []byte(`{"selfUpdate":"true"}`))
	require.NoError(t, err)
	assert.Equal(t, "true", eff.SelfUpdate)
}

func TestUpdateFileNullResetsSelfUpdate(t *testing.T) {
	base := t.TempDir()
	_, err := UpdateFile(base, []byte(`{"selfUpdate":"30m"}`))
	require.NoError(t, err)

	eff, err := UpdateFile(base, []byte(`{"selfUpdate":null}`))
	require.NoError(t, err)
	assert.Empty(t, eff.SelfUpdate)

	// The key is removed from the file so the built-in default (off) applies.
	stored := readStoredFile(t, base)
	assert.NotContains(t, stored, "selfUpdate")
}

func TestUpdateFileRejectsInvalidSelfUpdate(t *testing.T) {
	base := t.TempDir()
	// Unparseable durations are rejected at set time — the operator notices
	// the typo immediately instead of a daemon that silently never updates.
	_, err := UpdateFile(base, []byte(`{"selfUpdate":"banana"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selfUpdate")

	// A JSON bool is not the string grammar; reject it too.
	_, err = UpdateFile(base, []byte(`{"selfUpdate":true}`))
	require.Error(t, err)

	// Nothing was written by either failed attempt.
	_, statErr := os.Stat(mustPath(t, base))
	assert.True(t, os.IsNotExist(statErr))
}

func mustPath(t *testing.T, base string) string {
	t.Helper()
	p, err := ConfigPath(base)
	require.NoError(t, err)
	return p
}
