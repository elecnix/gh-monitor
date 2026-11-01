package prefs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestInterpolateOptionalSegment pins the optional-segment syntax: a clause
// wrapped in [[…]] is dropped when every token in it renders empty, and kept
// otherwise. This is what lets the built-in new-commit message name the
// co-authors without ever saying "co-authored by " (issue #179).
func TestInterpolateOptionalSegment(t *testing.T) {
	tests := []struct {
		name     string
		template string
		vars     map[string]string
		expected string
	}{
		{
			name:     "segment kept when its token has a value",
			template: "a[[ by {commitAuthor}]]b",
			vars:     map[string]string{"commitAuthor": "alice"},
			expected: "a by aliceb",
		},
		{
			name:     "segment dropped when its token is empty",
			template: "a[[ by {commitAuthor}]]b",
			vars:     map[string]string{"commitAuthor": ""},
			expected: "ab",
		},
		{
			name:     "segment dropped when its token is absent",
			template: "a[[ by {commitAuthor}]]b",
			vars:     map[string]string{},
			expected: "ab",
		},
		{
			name:     "segment kept when one of its tokens has a value",
			template: "a[[ {commitAuthor} and {commitCoauthors} ]]b",
			vars:     map[string]string{"commitAuthor": "alice", "commitCoauthors": ""},
			expected: "a alice and  b",
		},
		{
			name:     "segment dropped when none of its tokens has a value",
			template: "a[[ {commitAuthor} and {commitCoauthors} ]]b",
			vars:     map[string]string{"commitAuthor": "", "commitCoauthors": ""},
			expected: "ab",
		},
		{
			name:     "brackets without a recognized token stay literal",
			template: "a[[ always]]b",
			vars:     map[string]string{},
			expected: "a[[ always]]b",
		},
		{
			name:     "segment with an unknown token is kept so the typo stays visible",
			template: "a[[ by {bogus}]]b",
			vars:     map[string]string{},
			expected: "a[[ by {bogus}]]b",
		},
		{
			name:     "two segments are independent",
			template: "a[[ {commitAuthor} ]]b[[ {commitCoauthors} ]]c",
			vars:     map[string]string{"commitAuthor": "alice"},
			expected: "a alice bc",
		},
		{
			name:     "nested segments go together",
			template: "a[[ {commitAuthor}[[ {commitCoauthors}]] ]]b",
			vars:     map[string]string{"commitAuthor": "alice", "commitCoauthors": "bob"},
			expected: "a alice bob b",
		},
		{
			name:     "nested inner segment dropped but outer kept",
			template: "a[[ {commitAuthor}[[ {commitCoauthors}]] ]]b",
			vars:     map[string]string{"commitAuthor": "alice", "commitCoauthors": ""},
			expected: "a alice b",
		},
		{
			name:     "nested outer segment dropped with the inner one",
			template: "a[[ {commitAuthor}[[ {commitCoauthors}]] ]]b",
			vars:     map[string]string{"commitAuthor": "", "commitCoauthors": ""},
			expected: "ab",
		},
		{
			name:     "text outside a segment is untouched",
			template: "a {owner} b[[ {commitAuthor} ]] c",
			vars:     map[string]string{"owner": "elecnix", "commitAuthor": ""},
			expected: "a elecnix b c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, Interpolate(tt.template, tt.vars))
		})
	}
}

// TestInterpolateLeavesLiteralBracketsAlone pins that the optional-segment
// markers only mean something around a recognized {token}. A template that
// merely mentions brackets — a citation, a wiki link, a nested list marker —
// keeps reading the way its author wrote it.
func TestInterpolateLeavesLiteralBracketsAlone(t *testing.T) {
	vars := map[string]string{"prLabel": "owner/repo#1"}
	assert.Equal(t, "see [[1]] on owner/repo#1", Interpolate("see [[1]] on {prLabel}", vars))
	assert.Equal(t, "see [[note]] on owner/repo#1", Interpolate("see [[note]] on {prLabel}", vars))
	assert.Equal(t, "see [[{bogus}]] on owner/repo#1", Interpolate("see [[{bogus}]] on {prLabel}", vars))
}

// TestInterpolateNewCommitTemplate pins the built-in new-commit message
// (issue #179). It names the commit's co-authors, and the clause is omitted
// entirely when the commit carries no Co-authored-by trailer — never rendered
// as a dangling "co-authored by .". The author clause is omitted the same way
// when the author is unknown.
func TestInterpolateNewCommitTemplate(t *testing.T) {
	const (
		withCoauthor = "📝 New commit abc1234 pushed to owner/repo#42 by alice, co-authored by bob. " +
			"Review the PR description to ensure it still reflects the latest changes."
		withoutCoauthor = "📝 New commit abc1234 pushed to owner/repo#42 by alice. " +
			"Review the PR description to ensure it still reflects the latest changes."
		withoutAuthor = "📝 New commit abc1234 pushed to owner/repo#42, co-authored by bob. " +
			"Review the PR description to ensure it still reflects the latest changes."
		withoutEither = "📝 New commit abc1234 pushed to owner/repo#42. " +
			"Review the PR description to ensure it still reflects the latest changes."
	)

	base := map[string]string{
		"commitShortOid": "abc1234",
		"prLabel":        "owner/repo#42",
	}
	with := func(kv map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range kv {
			out[k] = v
		}
		return out
	}

	assert.Equal(t, withCoauthor, Interpolate(defaultTemplates["new-commit"],
		with(map[string]string{"commitAuthor": "alice", "commitCoauthors": "bob"})))
	assert.Equal(t, withoutCoauthor, Interpolate(defaultTemplates["new-commit"],
		with(map[string]string{"commitAuthor": "alice", "commitCoauthors": ""})))
	assert.Equal(t, withoutAuthor, Interpolate(defaultTemplates["new-commit"],
		with(map[string]string{"commitAuthor": "", "commitCoauthors": "bob"})))
	assert.Equal(t, withoutEither, Interpolate(defaultTemplates["new-commit"],
		with(map[string]string{"commitAuthor": "", "commitCoauthors": ""})))

	// A commit whose co-author list is absent from the vars entirely (a
	// backend that does not carry the field) reads the same as an empty
	// list: the clause is omitted, not rendered with a hole in it.
	got := Interpolate(defaultTemplates["new-commit"], with(map[string]string{"commitAuthor": "alice"}))
	assert.Equal(t, withoutCoauthor, got)
	assert.NotContains(t, got, "co-authored")
}

// TestNewCommitTemplateNamesEveryCoauthor pins that the built-in message
// names every co-author of the commit, comma-joined, and not just the first.
func TestNewCommitTemplateNamesEveryCoauthor(t *testing.T) {
	got := Interpolate(defaultTemplates["new-commit"], map[string]string{
		"commitShortOid":  "abc1234",
		"prLabel":         "owner/repo#42",
		"commitAuthor":    "alice",
		"commitCoauthors": "bob, carol",
	})
	assert.Contains(t, got, "co-authored by bob, carol")
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
			name: "optional segment accepted",
			prefs: Preferences{
				Templates: map[string]string{"merged": "done {prLabel}[[ by {commitAuthor}]]"},
			},
			wantErr: false,
		},
		{
			name: "unclosed optional segment rejected",
			prefs: Preferences{
				Templates: map[string]string{"merged": "done {prLabel}[[ by {commitAuthor}"},
			},
			wantErr: true,
		},
		{
			name: "unclosed nested optional segment rejected",
			prefs: Preferences{
				Templates: map[string]string{"merged": "done {prLabel}[[ a [[ b {commitAuthor}]]"},
			},
			wantErr: true,
		},
		{
			name: "unclosed brackets without a recognized token are literal text",
			prefs: Preferences{
				Templates: map[string]string{"merged": "done {prLabel}[[ by hand"},
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

func TestLoadDropsUnclosedSegment(t *testing.T) {
	base := t.TempDir()
	writeStored(t, base, `{"templates": {"merged": "done {prLabel}[[ by {commitAuthor}"}}`)

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

// TestSelfUpdateInterval pins the selfUpdate spec grammar (issue #82). It
// mirrors the semantics the removed GH_MONITOR_SELFUPDATE env var had, so a
// value written for the env var reads identically from the config file: off
// unless asked, "1"/"true" meaning the default cadence, and any positive Go
// duration an explicit one. Unparseable values are off — never guessed.
func TestSelfUpdateInterval(t *testing.T) {
	cases := []struct {
		spec string
		want time.Duration
	}{
		{"", 0},                   // absent: off
		{"0", 0},                  // explicit off
		{"false", 0},              // explicit off
		{"1", time.Hour},          // on: default cadence
		{"true", time.Hour},       // on: default cadence
		{"30m", 30 * time.Minute}, // custom cadence
		{"2h", 2 * time.Hour},     // custom cadence
		{"garbage", 0},            // unparseable: off, never guess
		{"-5m", 0},                // negative: off
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, SelfUpdateInterval(tc.spec, time.Hour), "spec %q", tc.spec)
	}
	// The default cadence is the caller's choice, not baked in.
	assert.Equal(t, 7*time.Minute, SelfUpdateInterval("1", 7*time.Minute))
}

func TestLoadSelfUpdate(t *testing.T) {
	base := t.TempDir()

	// Missing file: off (the default).
	got, err := Load(base)
	require.NoError(t, err)
	assert.Empty(t, got.SelfUpdate)

	writeStored(t, base, `{"selfUpdate": "30m"}`)
	got, err = Load(base)
	require.NoError(t, err)
	assert.Equal(t, "30m", got.SelfUpdate)

	// Absent key keeps the default even alongside other stored config.
	writeStored(t, base, `{"ignoredBots": ["dependabot"]}`)
	got, err = Load(base)
	require.NoError(t, err)
	assert.Empty(t, got.SelfUpdate)
	assert.Equal(t, []string{"dependabot"}, got.IgnoredBots)
}

// writeStored writes raw JSON to the preferences path under base.
func writeStored(t *testing.T, base, content string) {
	t.Helper()
	dir := filepath.Join(base, "gh-monitor")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(content), 0o644))
}
