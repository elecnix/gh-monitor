package prefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterpolate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		vars     map[string]string
		expected string
	}{
		{
			name:     "known tokens replaced",
			template: "{owner}/{repo}#{number}",
			vars:     map[string]string{"owner": "elecnix", "repo": "gh-monitor", "number": "42"},
			expected: "elecnix/gh-monitor#42",
		},
		{
			name:     "unknown token left literal",
			template: "{owner} {bogus}",
			vars:     map[string]string{"owner": "elecnix", "bogus": "x"},
			expected: "elecnix {bogus}",
		},
		{
			name:     "recognized-but-absent token left literal",
			template: "{owner} on {prLabel}",
			vars:     map[string]string{"owner": "elecnix"},
			expected: "elecnix on {prLabel}",
		},
		{
			name:     "no tokens passthrough",
			template: "just text",
			vars:     map[string]string{"owner": "x"},
			expected: "just text",
		},
		{
			name:     "repeated token",
			template: "{prLabel} {prLabel}",
			vars:     map[string]string{"prLabel": "PR#1"},
			expected: "PR#1 PR#1",
		},
		{
			name:     "empty value substitutes empty",
			template: "a{reviewAuthor}b",
			vars:     map[string]string{"reviewAuthor": ""},
			expected: "ab",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, Interpolate(tt.template, tt.vars))
		})
	}
}

func TestInterpolateDefaultTemplate(t *testing.T) {
	got := Interpolate(defaultTemplates["new-commit"], map[string]string{
		"commitShortOid": "abc1234",
		"prLabel":        "elecnix/gh-monitor#7",
		"commitAuthor":   "octocat",
	})
	assert.Contains(t, got, "abc1234")
	assert.Contains(t, got, "elecnix/gh-monitor#7")
	assert.Contains(t, got, "octocat")
}

func TestInterpolateConflictTemplate(t *testing.T) {
	// The conflict template must guide the agent to correlate conflicts
	// with CI failures, so it does not mistakenly assume an Actions outage.
	got := Interpolate(defaultTemplates["conflict"], map[string]string{
		"prLabel":       "elecnix/gh-monitor#7",
		"failingChecks": "CI / test, CI / lint",
	})
	assert.Contains(t, got, "⚠️")
	assert.Contains(t, got, "elecnix/gh-monitor#7")
	assert.Contains(t, got, "conflict")
	assert.Contains(t, got, "resolve the conflict first")
	assert.Contains(t, got, "causing the failures")

	// When failingChecks is empty, the template still reads cleanly.
	gotEmpty := Interpolate(defaultTemplates["conflict"], map[string]string{
		"prLabel":       "elecnix/gh-monitor#7",
		"failingChecks": "",
	})
	assert.Contains(t, gotEmpty, "⚠️")
	assert.Contains(t, gotEmpty, "conflict")
	assert.NotContains(t, gotEmpty, "()")
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		prefs   Preferences
		wantErr bool
	}{
		{
			name:    "defaults are valid",
			prefs:   DefaultPreferences(),
			wantErr: false,
		},
		{
			name: "unknown key rejected",
			prefs: Preferences{
				Templates: map[string]string{"bogus-key": "{prLabel}"},
			},
			wantErr: true,
		},
		{
			name: "templateless value rejected",
			prefs: Preferences{
				Templates: map[string]string{"merged": "no tokens here"},
			},
			wantErr: true,
		},
		{
			name: "known key with token accepted",
			prefs: Preferences{
				Templates: map[string]string{"merged": "done {prLabel}"},
			},
			wantErr: false,
		},
		{
			name: "non-template config exempt",
			prefs: Preferences{
				Templates:         map[string]string{},
				IgnoredBots:       []string{"dependabot"},
				RetriggerComments: true,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.prefs)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestConfigPathXDG(t *testing.T) {
	base := t.TempDir()
	path, err := ConfigPath(base)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "gh-monitor", "preferences.json"), path)
}

func TestConfigPathEnv(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	path, err := ConfigPath("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "gh-monitor", "preferences.json"), path)
}

func TestLoadDefaultsWhenMissing(t *testing.T) {
	base := t.TempDir()
	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, DefaultPreferences(), got)
}

func TestLoadOverlaysFile(t *testing.T) {
	base := t.TempDir()
	writeStored(t, base, `{
      "templates": {"merged": "custom {prLabel}"},
      "ignoredBots": ["dependabot"],
      "retriggerComments": true
    }`)

	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, "custom {prLabel}", got.Templates["merged"])
	// Untouched keys keep their defaults.
	assert.Equal(t, defaultTemplates["closed"], got.Templates["closed"])
	assert.Equal(t, []string{"dependabot"}, got.IgnoredBots)
	assert.True(t, got.RetriggerComments)
}

func TestLoadDropsTemplatelessValue(t *testing.T) {
	base := t.TempDir()
	writeStored(t, base, `{"templates": {"merged": "no tokens"}}`)

	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, defaultTemplates["merged"], got.Templates["merged"])
}

func TestLoadNullResetsToDefault(t *testing.T) {
	base := t.TempDir()
	writeStored(t, base, `{"templates": {"merged": null}}`)

	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, defaultTemplates["merged"], got.Templates["merged"])
}

func TestLoadIgnoresUnknownKey(t *testing.T) {
	base := t.TempDir()
	writeStored(t, base, `{"templates": {"bogus-key": "{prLabel}"}}`)

	got, err := Load(base)
	require.NoError(t, err)
	_, present := got.Templates["bogus-key"]
	assert.False(t, present)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	base := t.TempDir()
	want := DefaultPreferences()
	want.Templates["merged"] = "🎉 {prLabel} merged"
	want.IgnoredBots = []string{"renovate", "dependabot"}
	want.RetriggerComments = true

	require.NoError(t, Save(base, want))

	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSaveCreatesDirWithMode(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, Save(base, DefaultPreferences()))

	path, err := ConfigPath(base)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, Save(base, DefaultPreferences()))

	dir := filepath.Join(base, "gh-monitor")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "temp file left behind: %s", e.Name())
	}
	assert.Len(t, entries, 1)
	assert.Equal(t, "preferences.json", entries[0].Name())
}

func TestTemplateKeysComplete(t *testing.T) {
	assert.Len(t, TemplateKeys(), len(defaultTemplates))
	assert.Contains(t, TemplateKeys(), "first-poll")
	assert.Contains(t, TemplateKeys(), "all-clear")
}

func TestLoadFallsBackToLegacyPath(t *testing.T) {
	base := t.TempDir()

	// Write to the LEGACY path only (gh-pr-monitor).
	legDir := filepath.Join(base, "gh-pr-monitor")
	require.NoError(t, os.MkdirAll(legDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legDir, "preferences.json"),
		[]byte(`{"templates":{"merged":"legacy {prLabel}"}}`), 0o644))

	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, "legacy {prLabel}", got.Templates["merged"])
}

func TestLoadPrefersNewPathOverLegacy(t *testing.T) {
	base := t.TempDir()

	// Write to the legacy path.
	legDir := filepath.Join(base, "gh-pr-monitor")
	require.NoError(t, os.MkdirAll(legDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legDir, "preferences.json"),
		[]byte(`{"templates":{"merged":"legacy {prLabel}"}}`), 0o644))

	// Also write to the new path (gh-monitor).
	newDir := filepath.Join(base, "gh-monitor")
	require.NoError(t, os.MkdirAll(newDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(newDir, "preferences.json"),
		[]byte(`{"templates":{"merged":"new {prLabel}"}}`), 0o644))

	got, err := Load(base)
	require.NoError(t, err)
	assert.Equal(t, "new {prLabel}", got.Templates["merged"])
}

// writeStored writes raw JSON to the preferences path under base.
func writeStored(t *testing.T, base, content string) {
	t.Helper()
	dir := filepath.Join(base, "gh-monitor")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(content), 0o644))
}
