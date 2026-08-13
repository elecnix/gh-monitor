package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionStringFormats(t *testing.T) {
	tests := []struct {
		name  string
		build buildDetails
		want  string
	}{
		{
			name: "released build reports tag, revision, time and toolchain",
			build: buildDetails{
				Version:   "v1.17.0",
				Revision:  "55d91542872d9f87f92d5528fd7d6450257ff770",
				Time:      "2026-08-11T15:24:54Z",
				GoVersion: "go1.22.12",
			},
			want: "gh monitor v1.17.0 (55d9154, 2026-08-11T15:24:54Z, go1.22.12)",
		},
		{
			name: "dirty tree is called out next to the revision",
			build: buildDetails{
				Version:   "v1.17.0",
				Revision:  "55d91542872d9f87f92d5528fd7d6450257ff770",
				Time:      "2026-08-11T15:24:54Z",
				Modified:  true,
				GoVersion: "go1.22.12",
			},
			want: "gh monitor v1.17.0 (55d9154-dirty, 2026-08-11T15:24:54Z, go1.22.12)",
		},
		{
			name: "a build with the VCS stamp stripped says so instead of lying",
			build: buildDetails{
				Version:   devel,
				GoVersion: "go1.22.12",
			},
			want: "gh monitor (devel) (unknown revision, go1.22.12)",
		},
		{
			name: "a missing build time is omitted rather than printed empty",
			build: buildDetails{
				Version:   devel,
				Revision:  "55d91542872d9f87f92d5528fd7d6450257ff770",
				GoVersion: "go1.22.12",
			},
			want: "gh monitor (devel) (55d9154, go1.22.12)",
		},
		{
			name:  "an unstamped, info-less build still prints something honest",
			build: buildDetails{GoVersion: "go1.22.12"},
			want:  "gh monitor (devel) (unknown revision, go1.22.12)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, versionString(tc.build))
		})
	}
}

func TestReadBuildDetailsNeverReportsAnEmptyVersion(t *testing.T) {
	d := readBuildDetails()
	assert.NotEmpty(t, d.Version)
	assert.NotEmpty(t, d.GoVersion)
	assert.NotEmpty(t, versionString(d))
}

func TestVersionFlagPrintsTheVersion(t *testing.T) {
	root := newRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--version"})

	require.NoError(t, root.Execute())
	assert.Equal(t, versionString(readBuildDetails())+"\n", buf.String())
}

func TestVersionSubcommandPrintsTheSameString(t *testing.T) {
	root := newRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"version"})

	require.NoError(t, root.Execute())
	assert.Equal(t, versionString(readBuildDetails())+"\n", buf.String())
}

// The default command takes a positional PR selector, so a `version`
// subcommand could in principle steal an argument that used to reach the
// monitor. It cannot: cobra only resolves the first positional as a
// subcommand, and "version" was never a valid selector — it errored out.
func TestVersionSubcommandDoesNotShadowThePullRequestSelector(t *testing.T) {
	root := newRootCommand()

	target, _, err := root.Find([]string{"42"})
	require.NoError(t, err)
	assert.Same(t, root, target, "a PR number must still reach the default command")

	target, _, err = root.Find([]string{"https://github.com/o/r/pull/42"})
	require.NoError(t, err)
	assert.Same(t, root, target, "a PR URL must still reach the default command")

	target, _, err = root.Find([]string{"version"})
	require.NoError(t, err)
	assert.Equal(t, "version", target.Name())
}
