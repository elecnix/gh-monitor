package reactions

import (
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateValidReactions(t *testing.T) {
	for name := range ValidReactions {
		t.Run(name, func(t *testing.T) {
			err := Validate(name)
			require.NoError(t, err)
		})
	}
}

func TestValidateInvalidReaction(t *testing.T) {
	err := Validate("not_a_reaction")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid reaction")
}

func TestValidateEmptyReaction(t *testing.T) {
	err := Validate("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid reaction")
}

func TestValidReactionNamesSorted(t *testing.T) {
	names := ValidReactionNames()
	require.NotEmpty(t, names)
	for i := 1; i < len(names); i++ {
		assert.True(t, names[i-1] < names[i],
			"expected sorted order, got %q before %q", names[i-1], names[i])
	}
}

func TestValidReactionNamesContainsAll(t *testing.T) {
	names := ValidReactionNames()
	assert.Len(t, names, len(ValidReactions))

	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	for key := range ValidReactions {
		assert.True(t, set[key], "missing key %q in ValidReactionNames()", key)
	}
}

func TestReactInvalidReaction(t *testing.T) {
	err := React(nil, "node123", "not_a_reaction")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid reaction")
}

func TestValidReactionsMapIsComplete(t *testing.T) {
	// All expected reaction types must be present
	expected := []string{
		"thumbs_up", "thumbs_down", "laugh", "hooray",
		"confused", "heart", "rocket", "eyes",
	}
	for _, e := range expected {
		_, ok := ValidReactions[e]
		assert.True(t, ok, "missing reaction %q", e)
	}
}

// TestValidReactionsMatchTheBackendVocabulary is the drift guard for the
// reaction contract. The names are the protocol: what the CLI accepts, what
// ReactionActor.React carries, and what an out-of-process backend receives on
// the wire. backend exports them because internal/ is not importable from
// outside this module, so the two tables have to stay identical — a name
// added here alone would be a vocabulary this module accepts and no server can
// look up.
func TestValidReactionsMatchTheBackendVocabulary(t *testing.T) {
	assert.Equal(t, backend.ReactionNames(), ValidReactionNames(),
		"the internal name table and the exported protocol vocabulary must be the same list")
	for name := range ValidReactions {
		assert.True(t, backend.ValidReaction(name),
			"%q is accepted here but absent from backend.ReactionNames()", name)
	}
}

// TestGraphQLEnumIsTheUppercasedWireName pins the only transformation this
// module applies. If a future name ever needs a different mapping, this test
// fails on purpose so the exception is written down rather than discovered by a
// server author reading the wrong table.
func TestGraphQLEnumIsTheUppercasedWireName(t *testing.T) {
	for name, enum := range ValidReactions {
		assert.Equal(t, strings.ToUpper(name), enum,
			"reaction %q maps to %q, not its uppercased name", name, enum)
	}
}
