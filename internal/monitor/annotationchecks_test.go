package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnnotationChecks_Allows exercises the predicate directly.
//
// The cases are table-driven: each row pairs an allowlist with the check
// name it is asked about and the answer it must give.
func TestAnnotationChecks_Allows(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		check string
		want  bool
	}{
		{"a listed check passes", []string{"dupdelta"}, "dupdelta", true},
		{"an unlisted check does not", []string{"dupdelta"}, "lint", false},
		{"an empty check name does not", []string{"dupdelta"}, "", false},
		{"matching ignores case", []string{"  DupDelta  "}, "DUPDELTA", true},
		{"matching ignores case, the other way", []string{"  DupDelta  "}, "DupDelta", true},
		{"the whole name has to match", []string{"lint"}, "lint (node 20)", false},
		{"a listed name is matched whole too", []string{"lint (node 20)"}, "lint", false},
		{"several names can be listed", []string{"dupdelta", "lint"}, "lint", true},
		{"a third check is still refused", []string{"dupdelta", "lint"}, "build", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewAnnotationChecks(tc.allow...)
			assert.Equal(t, tc.want, f.Allows(tc.check))
		})
	}
}

// TestAnnotationChecks_NilFilterAllowsEveryCheck confirms a nil filter — the
// default — admits every check, whatever it is named.
func TestAnnotationChecks_NilFilterAllowsEveryCheck(t *testing.T) {
	var nilFilter *AnnotationChecks
	assert.True(t, nilFilter.Allows("dupdelta"))
	assert.True(t, nilFilter.Allows("anything at all"))
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
	// Both spellings of "no check named": an empty string, and a string of
	// nothing but separators.
	for _, input := range []string{"", "  , , "} {
		f, err := ParseAnnotationChecks(input)
		require.NoError(t, err)
		assert.Nil(t, f, "input %q must yield a nil filter", input)
		assert.True(t, f.Allows("dupdelta"), "input %q must keep every check", input)
	}
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
