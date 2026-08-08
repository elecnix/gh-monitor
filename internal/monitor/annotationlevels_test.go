package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnnotationLevels_Allows exercises the predicate directly.
//
// Cases:
//   - nil filter allows everything (default: warning + failure).
//   - a non-nil allowlist allows only the listed levels.
//   - an empty allowlist ("none") allows nothing.
//   - matching is case-insensitive and trims whitespace.
func TestAnnotationLevels_Allows(t *testing.T) {
	// nil filter = default behaviour (warning + failure).
	var nilFilter *AnnotationLevels
	assert.True(t, nilFilter.Allows("WARNING"))
	assert.True(t, nilFilter.Allows("FAILURE"))
	assert.False(t, nilFilter.Allows("NOTICE"))

	// non-nil allowlist: only listed levels pass.
	f := NewAnnotationLevels("warning", "failure")
	assert.True(t, f.Allows("WARNING"))
	assert.True(t, f.Allows("FAILURE"))
	assert.False(t, f.Allows("NOTICE"))
	assert.False(t, f.Allows(""))

	// notice only.
	f2 := NewAnnotationLevels("notice")
	assert.True(t, f2.Allows("NOTICE"))
	assert.False(t, f2.Allows("WARNING"))
	assert.False(t, f2.Allows("FAILURE"))

	// empty allowlist ("none") = allow nothing.
	empty := NewAnnotationLevels()
	assert.False(t, empty.Allows("WARNING"))
	assert.False(t, empty.Allows("NOTICE"))
	assert.False(t, empty.Allows("FAILURE"))

	// case-insensitive + whitespace-trimmed.
	mixed := NewAnnotationLevels("  Warning  ")
	assert.True(t, mixed.Allows("WARNING"))
	assert.True(t, mixed.Allows("warning"))
	assert.True(t, mixed.Allows("Warning"))
}

// TestParseAnnotationLevels_Valid exercises the parser with valid inputs.
func TestParseAnnotationLevels_Valid(t *testing.T) {
	// Single value.
	f, err := ParseAnnotationLevels("warning")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.True(t, f.Allows("WARNING"))
	assert.False(t, f.Allows("FAILURE"))

	// Comma-separated list with whitespace and empties.
	f, err = ParseAnnotationLevels(" notice , warning ,, FAILURE ")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.True(t, f.Allows("NOTICE"))
	assert.True(t, f.Allows("WARNING"))
	assert.True(t, f.Allows("FAILURE"))

	// "none" is a valid value that allows nothing.
	f, err = ParseAnnotationLevels("none")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.False(t, f.Allows("WARNING"))
	assert.False(t, f.Allows("FAILURE"))
	assert.False(t, f.Allows("NOTICE"))

	// Mixed case.
	f, err = ParseAnnotationLevels("Notice, Warning, Failure")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.True(t, f.Allows("NOTICE"))
	assert.True(t, f.Allows("WARNING"))
	assert.True(t, f.Allows("FAILURE"))
}

// TestParseAnnotationLevels_UnknownRejects confirms an unrecognised level is a
// hard error naming the valid set, rather than silently ignored.
func TestParseAnnotationLevels_UnknownRejects(t *testing.T) {
	_, err := ParseAnnotationLevels("warning,error")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown annotation level")
	assert.Contains(t, err.Error(), "error")
	assert.Contains(t, err.Error(), "notice")
	assert.Contains(t, err.Error(), "warning")
	assert.Contains(t, err.Error(), "failure")
	assert.Contains(t, err.Error(), "none")
}

// TestParseAnnotationLevels_Default confirms that the omitted-flag case
// (NewAnnotationLevels("warning","failure")) is equivalent to the default.
func TestParseAnnotationLevels_Default(t *testing.T) {
	// The constructor used in the CLI when the flag is not passed.
	def := NewAnnotationLevels("warning", "failure")
	assert.True(t, def.Allows("WARNING"))
	assert.True(t, def.Allows("FAILURE"))
	assert.False(t, def.Allows("NOTICE"))
}

// TestParseAnnotationLevels_EmptyInputDefault confirms an empty input string
// returns a filter allowing warning + failure (the default), not a "mute all"
// filter. This differs from EventFilter's behaviour because the annotation
// flag is an opt-in on top of a sensible default.
func TestParseAnnotationLevels_EmptyInputDefault(t *testing.T) {
	f, err := ParseAnnotationLevels("")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.True(t, f.Allows("WARNING"))
	assert.True(t, f.Allows("FAILURE"))
	assert.False(t, f.Allows("NOTICE"))
}

// TestAnnotationLevels_String confirms the diagnostic rendering.
func TestAnnotationLevels_String(t *testing.T) {
	var nilFilter *AnnotationLevels
	assert.Equal(t, "<default>", nilFilter.String())

	empty := NewAnnotationLevels()
	assert.Equal(t, "<none>", empty.String())

	populated := NewAnnotationLevels("warning", "failure")
	assert.Equal(t, "failure,warning", populated.String())
}
