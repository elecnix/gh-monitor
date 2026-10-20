package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeSelector(t *testing.T) {
	selector, err := NormalizeSelector("https://github.com/octo/demo/pull/7", 7)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/octo/demo/pull/7", selector)

	selector, err = NormalizeSelector("7", 7)
	require.NoError(t, err)
	assert.Equal(t, "7", selector)

	selector, err = NormalizeSelector("", 42)
	require.NoError(t, err)
	assert.Equal(t, "42", selector)

	_, err = NormalizeSelector("https://github.com/octo/demo/pull/7", 8)
	require.Error(t, err)

	_, err = NormalizeSelector("octo/demo#7", 0)
	require.NoError(t, err)
}

func TestNormalizeSelectorShorthand(t *testing.T) {
	// The OWNER/REPO#NUMBER form is accepted as a selector on its own.
	selector, err := NormalizeSelector("octo/demo#7", 0)
	require.NoError(t, err)
	assert.Equal(t, "octo/demo#7", selector)

	// A matching --pr is accepted, exactly as it is for a URL selector.
	selector, err = NormalizeSelector("octo/demo#7", 7)
	require.NoError(t, err)
	assert.Equal(t, "octo/demo#7", selector)

	// A mismatching --pr is rejected, exactly as it is for a URL selector.
	_, err = NormalizeSelector("octo/demo#7", 8)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match --pr=8")
}

func TestNormalizeSelectorRejectsUnknownForm(t *testing.T) {
	// The error message names the accepted forms, so a reader who typed the
	// shorthand before this change knows what to try.
	_, err := NormalizeSelector("octo/demo#7x", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner/repo#number")
}

func TestResolveShorthand(t *testing.T) {
	id, err := Resolve("octo/demo#9", "", "")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 9, Target: "pr"}, id)
}

func TestResolveShorthandHost(t *testing.T) {
	// The shorthand carries no host: it names a repository on the host the
	// caller is already talking to.
	id, err := Resolve("octo/demo#9", "", "ghe.example.com")
	require.NoError(t, err)
	assert.Equal(t, "ghe.example.com", id.Host)

	id, err = Resolve("octo/demo#9", "", "")
	require.NoError(t, err)
	assert.Equal(t, "github.com", id.Host)
}

func TestResolveShorthandOverridesRepoFlag(t *testing.T) {
	// A selector that names its own repository wins over --repo, the same
	// way a pull request URL's repository does.
	id, err := Resolve("octo/demo#9", "other/repo", "github.com")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 9, Target: "pr"}, id)
}

func TestResolveShorthandNeedsNoRepoFlag(t *testing.T) {
	// The whole point of the shorthand: no -R, no git context, no error.
	id, err := Resolve("octo/demo#9", "", "")
	require.NoError(t, err)
	assert.Equal(t, "octo", id.Owner)
	assert.Equal(t, "demo", id.Repo)
}

func TestResolveShorthandRejectsMalformed(t *testing.T) {
	for _, selector := range []string{
		"octo/demo",   // no number: not a pull request selector
		"octo/demo#",  // no number
		"octo#9",      // no repository
		"#9",          // no owner or repository
		"octo/demo#0", // not a pull request number
	} {
		t.Run(selector, func(t *testing.T) {
			_, err := Resolve(selector, "octo/demo", "github.com")
			require.Error(t, err)
		})
	}
}

func TestResolveURL(t *testing.T) {
	id, err := Resolve("https://github.com/octo/demo/pull/9", "", "")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 9, Target: "pr"}, id)
}

func TestResolveHostSanitization(t *testing.T) {
	id, err := Resolve("11", "octo/demo", "HTTPS://GHE.EXAMPLE.COM:8443/")
	require.NoError(t, err)
	assert.Equal(t, "ghe.example.com", id.Host)
}

func TestResolveURLHostPrecedence(t *testing.T) {
	id, err := Resolve("https://git.enterprise.local:8443/octo/demo/pull/13", "", "github.com")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "git.enterprise.local", Number: 13, Target: "pr"}, id)
}

func TestResolveNumberRequiresRepo(t *testing.T) {
	_, err := Resolve("7", "", "")
	require.Error(t, err)

	id, err := Resolve("7", "octo/demo", "github.com")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 7, Target: "pr"}, id)
}

func TestResolveNumberMissingRepoPrefix(t *testing.T) {
	_, err := Resolve("7", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--repo:")
}

func TestResolveNumberInvalidRepoPrefix(t *testing.T) {
	_, err := Resolve("7", "not-a-repo", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--repo:")
}

func TestResolveRef(t *testing.T) {
	id, err := ResolveRef("main", "octo/demo", "")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Target: "ref", Ref: "main"}, id)

	id, err = ResolveRef("refs/heads/feature/branch", "octo/demo", "github.com")
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/feature/branch", id.Ref)
	assert.Equal(t, "ref", id.Target)

	// Empty ref is rejected.
	_, err = ResolveRef("", "octo/demo", "")
	require.Error(t, err)

	// Missing repo is rejected.
	_, err = ResolveRef("main", "", "")
	require.Error(t, err)
}

func TestResolveCommit(t *testing.T) {
	id, err := ResolveCommit("abc123def456", "octo/demo", "")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Target: "commit", CommitSHA: "abc123def456"}, id)

	// Empty SHA is rejected.
	_, err = ResolveCommit("", "octo/demo", "")
	require.Error(t, err)

	// Missing repo is rejected.
	_, err = ResolveCommit("abc123", "", "")
	require.Error(t, err)
}

func TestResolveIssue(t *testing.T) {
	id, err := ResolveIssue(42, "octo/demo", "")
	require.NoError(t, err)
	assert.Equal(t, Identity{Owner: "octo", Repo: "demo", Host: "github.com", Target: "issue", Number: 42}, id)

	// Zero number is rejected.
	_, err = ResolveIssue(0, "octo/demo", "")
	require.Error(t, err)

	// Missing repo is rejected.
	_, err = ResolveIssue(7, "", "")
	require.Error(t, err)
}

func TestSanitizeHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", "github.com"},
		{"bare", "github.com", "github.com"},
		{"https scheme", "https://github.com", "github.com"},
		{"http scheme", "http://github.com", "github.com"},
		{"scheme with path", "https://github.com/some/path", "github.com"},
		{"with port", "ghe.example.com:8443", "ghe.example.com"},
		{"scheme with port", "https://ghe.example.com:8443", "ghe.example.com"},
		{"mixed case", "HTTPS://GitHub.Com", "github.com"},
		{"trailing slash", "https://github.com/", "github.com"},
		{"whitespace", "  github.com  ", "github.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHost(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}
