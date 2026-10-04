package backend

import (
	"regexp"
	"testing"
)

// wireName is the shape every reaction name must have: lower_snake_case, as
// typed at the CLI. An API's own token ("THUMBS_UP", "thumbsup") would not
// match, which is the point.
var wireName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// The reaction names are a protocol contract: they are what the CLI accepts,
// what ReactionActor.React takes, and what crosses the wire to an
// out-of-process backend. These tests pin the properties a server can rely on
// without reading this module's internals.

func TestReactionNamesAreSortedAndUnique(t *testing.T) {
	names := ReactionNames()
	if len(names) == 0 {
		t.Fatal("the reaction vocabulary must not be empty")
	}
	seen := make(map[string]bool, len(names))
	for i, n := range names {
		if seen[n] {
			t.Fatalf("duplicate reaction name %q", n)
		}
		seen[n] = true
		if i > 0 && names[i-1] >= n {
			t.Fatalf("expected canonical order, got %q before %q", names[i-1], n)
		}
	}
}

func TestReactionNamesAreCLISpellingsNotAPITokens(t *testing.T) {
	// The wire carries the name the user types. A server that expected an
	// API-native token instead — "THUMBS_UP", "thumbsup", "THUMBSUP" — would
	// silently reject every real request, so the shape is asserted rather than
	// assumed.
	for _, n := range ReactionNames() {
		if !wireName.MatchString(n) {
			t.Errorf("%q is not a lower_snake_case name: the wire spelling is the CLI spelling, not an API token", n)
		}
	}
}

func TestEveryReactionConstantIsInTheVocabulary(t *testing.T) {
	listed := make(map[string]bool)
	for _, n := range ReactionNames() {
		listed[n] = true
	}
	for _, c := range []string{
		ReactionThumbsUp, ReactionThumbsDown, ReactionLaugh, ReactionHooray,
		ReactionConfused, ReactionHeart, ReactionRocket, ReactionEyes,
	} {
		if !listed[c] {
			t.Errorf("constant %q is missing from ReactionNames()", c)
		}
	}
}

func TestValidReaction(t *testing.T) {
	for _, n := range ReactionNames() {
		if !ValidReaction(n) {
			t.Errorf("ValidReaction(%q) = false, want true", n)
		}
	}
	for _, bad := range []string{
		"", "not_a_reaction", "THUMBS_UP", "thumbsup", "thumbs up", "Thumbs_Up", "eyes,",
	} {
		if ValidReaction(bad) {
			t.Errorf("ValidReaction(%q) = true, want false", bad)
		}
	}
}

func TestReactionNamesReturnsACopy(t *testing.T) {
	// AllKinds makes the same promise: the canonical slice belongs to the
	// package, so a caller sorting or truncating it in place must not be able
	// to corrupt the vocabulary for everyone else.
	first := ReactionNames()
	if len(first) == 0 {
		t.Fatal("the reaction vocabulary must not be empty")
	}
	first[0] = "mutated"
	if again := ReactionNames(); again[0] == "mutated" {
		t.Fatal("a caller mutated the shared reaction vocabulary")
	}
}
