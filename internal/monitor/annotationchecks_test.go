package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnnotationChecks_Allows exercises the predicate directly.
//
// Cases:
//   - nil filter allows every check (the default).
//   - a non-nil allowlist allows only the listed check names.
//   - matching is case-insensitive and trims whitespace.
//   - the match is on the WHOLE name, so a prefix of a check name does not
//     pass: that is what keeps "lint" from matching "lint (node 20)".
func TestAnnotationChecks_Allows(t *testing.T) {
	// nil filter = every check.
	var nilFilter *AnnotationChecks
	assert.True(t, nilFilter.Allows("dupdelta"))
	assert.True(t, nilFilter.Allows("anything at all"))

	// non-nil allowlist: only the listed names pass.
	f := NewAnnotationChecks("dupdelta")
	assert.True(t, f.Allows("dupdelta"))
	assert.False(t, f.Allows("lint"))
	assert.False(t, f.Allows(""))

	// case-insensitive + whitespace-trimmed.
	mixed := NewAnnotationChecks("  DupDelta  ")
	assert.True(t, mixed.Allows("DUPDELTA"))
	assert.True(t, mixed.Allows("dupdelta"))
	assert.True(t, mixed.Allows("DupDelta"))

	// The whole name has to match: "lint" does not admit "lint (node 20)",
	// and "lint (node 20)" does not admit "lint".
	whole := NewAnnotationChecks("lint")
	assert.True(t, whole.Allows("lint"))
	assert.False(t, whole.Allows("lint (node 20)"))

	// Several names at once.
	multi := NewAnnotationChecks("dupdelta", "lint")
	assert.True(t, multi.Allows("dupdelta"))
	assert.True(t, multi.Allows("lint"))
	assert.False(t, multi.Allows("build"))
}

// TestAnnotationChecks_None exercises the explicit "none" spelling: a
// filter that allows nothing, whichever other names accompany it.
func TestAnnotationChecks_None(t *testing.T) {
	f := NewAnnotationChecks("none")
	assert.False(t, f.Allows("dupdelta"))
	assert.False(t, f.Allows("anything"))
	assert.True(t, f.IsNone())

	// "none" wins over the other names in the list.
	both := NewAnnotationChecks("dupdelta", "none")
	assert.False(t, both.Allows("dupdelta"))
	assert.True(t, both.IsNone())

	// An empty allowlist allows nothing either — the same "nothing to
	// report" state, reached without naming it.
	empty := NewAnnotationChecks()
	assert.False(t, empty.Allows("dupdelta"))
	assert.True(t, empty.IsNone())
}

// TestParseAnnotationChecks_Valid exercises the parser with valid inputs.
//
// The cases are table-driven rather than a run of near-identical blocks: the
// parser's job is to turn a string into a filter, so the interesting
// property of each input is the set of check names it admits.
func TestParseAnnotationChecks_Valid(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"single value", "dupdelta", []string{"dupdelta"}},
		{"several values", "dupdelta,lint", []string{"dupdelta", "lint"}}, {"whitespace and empties are dropped", " dupdelta , lint ,, CI / test ", []string{"ci / test", "dupdelta", "lint"}},
		{"mixed case is folded", "DupDelta,Lint", []string{"dupdelta", "lint"}},
		{"a check name keeps its spaces", "CI / test", []string{"ci / test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseAnnotationChecks(tc.input)
			require.NoError(t, err)
			require.NotNil(t, f)
			assert.Equal(t, tc.want, f.Names())
			for _, n := range tc.want {
				assert.True(t, f.Allows(n), "%q must be allowed", n)
			}
		})
	}

	// "none" is a valid value that allows nothing.
	f, err := ParseAnnotationChecks("none")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.False(t, f.Allows("dupdelta"))
	assert.False(t, f.Allows("lint"))
}

// TestParseAnnotationChecks_EmptyInputKeepsEveryCheck confirms an empty input
// string returns a nil filter — "every check" — not a "mute all" filter.
//
// This is the opposite of the level filter's grammar, and deliberately so: an
// empty level list means "report no annotations" because that is what an
// operator asking for levels wants to be able to say, while an empty CHECK
// list has no such reading. Silently muting every annotation because a
// preference file listed no checks would be the worst possible reading of a
// missing setting.
func TestParseAnnotationChecks_EmptyInputKeepsEveryCheck(t *testing.T) {
	f, err := ParseAnnotationChecks("")
	require.NoError(t, err)
	assert.Nil(t, f)
	assert.True(t, f.Allows("dupdelta"))

	// A string of nothing but separators is the same case.
	f, err = ParseAnnotationChecks("  , , ")
	require.NoError(t, err)
	assert.Nil(t, f)
	assert.True(t, f.Allows("dupdelta"))
}

// TestAnnotationChecks_Names confirms the filter can cross a process
// boundary: the names it reports rebuild it.
func TestAnnotationChecks_Names(t *testing.T) {
	// A nil filter reports nil — "every check".
	var nilFilter *AnnotationChecks
	assert.Nil(t, nilFilter.Names())

	// The names come back sorted, so the same filter always produces the
	// same wire form.
	f := NewAnnotationChecks("lint", "dupdelta")
	assert.Equal(t, []string{"dupdelta", "lint"}, f.Names())

	// A filter that allows nothing reports the "none" spelling, because an
	// empty slice would read as "every check" on the far side and quietly
	// turn the filter off.
	none := NewAnnotationChecks("none")
	assert.Equal(t, []string{"none"}, none.Names())

	// The round trip holds: the names rebuild an equivalent filter.
	rebuilt, err := ParseAnnotationChecks(joinNames(f.Names()))
	require.NoError(t, err)
	require.NotNil(t, rebuilt)
	assert.True(t, rebuilt.Allows("dupdelta"))
	assert.True(t, rebuilt.Allows("lint"))
	assert.False(t, rebuilt.Allows("build"))
}

// TestAnnotationChecks_String renders the filter for logs and help text.
func TestAnnotationChecks_String(t *testing.T) {
	var nilFilter *AnnotationChecks
	assert.Equal(t, "<all>", nilFilter.String())
	assert.Equal(t, "none", NewAnnotationChecks().String())
	assert.Equal(t, "none", NewAnnotationChecks("none").String())
	assert.Equal(t, "dupdelta,lint", NewAnnotationChecks("lint", "dupdelta").String())
}

// joinNames renders a name list the way the wire carries it.
func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}
