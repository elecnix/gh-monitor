package prefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigDir_MatchesEveryDerivation is the guard for the duplicated
// derivation this function exists to remove. Two call sites used to build the
// config directory by hand from ConfigPath — one with filepath.Dir, one with
// strings.TrimSuffix of separator+"preferences.json" — with nothing tying them
// together. Any of these assertions failing means a rename of preferences.json
// or a change to ConfigPath would silently split on-disk state (cursor stores,
// event log) across two directories.
func TestConfigDir_MatchesEveryDerivation(t *testing.T) {
	bases := []string{
		"",
		t.TempDir(),
		filepath.Join(t.TempDir(), "xdg"),
		// A path whose parent happens to end in "gh-monitor" catches a
		// derivation that assumes a fixed depth.
		filepath.Join(t.TempDir(), "gh-monitor", "nested"),
	}
	for _, base := range bases {
		name := base
		if name == "" {
			name = "<default>"
		}
		t.Run(name, func(t *testing.T) {
			dir, err := ConfigDir(base)
			require.NoError(t, err)

			path, err := ConfigPath(base)
			require.NoError(t, err)

			// The hand-rolled spellings the two call sites used, kept here
			// verbatim so the test fails if either ever drifts.
			assert.Equal(t, dir, filepath.Dir(path),
				"ConfigDir must equal filepath.Dir(ConfigPath)")
			assert.Equal(t, dir, strings.TrimSuffix(path, string(filepath.Separator)+"preferences.json"),
				"ConfigDir must equal the TrimSuffix spelling")

			// Sanity: the file actually lives directly in the directory.
			assert.Equal(t, "preferences.json", filepath.Base(path))
			assert.Equal(t, dir, filepath.Dir(path))
		})
	}
}

// TestConfigDir_DefaultBaseFollowsXDG pins the documented fallback chain —
// explicit base, then XDG_CONFIG_HOME, then $HOME/.config — because every
// caller that passes "" lands on it.
func TestConfigDir_DefaultBaseFollowsXDG(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)

	dir, err := ConfigDir("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "gh-monitor"), dir)

	path, err := ConfigPath("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "preferences.json"), path)

	// With XDG unset the home fallback applies, so the directory still sits
	// beside the preferences file rather than at the filesystem root.
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := os.UserHomeDir(); err != nil {
		t.Skipf("home directory unresolvable on this platform: %v", err)
	}
	dir, err = ConfigDir("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".config", "gh-monitor"), dir)
}
