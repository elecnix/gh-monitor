package reactions

import (
	"fmt"
	"sort"
	"strings"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
)

const addReactionMutation = `
mutation AddReaction($subjectId: ID!, $content: ReactionContent!) {
  addReaction(input: {subjectId: $subjectId, content: $content}) {
    reaction {
      content
    }
  }
}
`

// ValidReactions maps each reaction name to GitHub's GraphQL ReactionContent
// enum value.
//
// The names are the protocol contract, not a CLI convention: the same strings
// are what ReactionActor.React takes and what reaches an out-of-process
// backend on the wire, which is why backend exports them too (ReactionNames).
// Only the values are GitHub-specific — a different backend maps the names to
// its own API's tokens. A test holds the two lists identical.
var ValidReactions = map[string]string{
	backend.ReactionThumbsUp:   "THUMBS_UP",
	backend.ReactionThumbsDown: "THUMBS_DOWN",
	backend.ReactionLaugh:      "LAUGH",
	backend.ReactionHooray:     "HOORAY",
	backend.ReactionConfused:   "CONFUSED",
	backend.ReactionHeart:      "HEART",
	backend.ReactionRocket:     "ROCKET",
	backend.ReactionEyes:       "EYES",
}

// ValidReactionNames returns a sorted list of valid reaction names for display.
func ValidReactionNames() []string {
	names := make([]string, 0, len(ValidReactions))
	for k := range ValidReactions {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Validate checks if the given reaction name is valid and returns an error if not.
func Validate(reaction string) error {
	if _, ok := ValidReactions[reaction]; !ok {
		return fmt.Errorf("invalid reaction %q; valid values: %s", reaction, strings.Join(ValidReactionNames(), ", "))
	}
	return nil
}

// React adds a reaction to any reactable GitHub node.
// React adds a reaction to any reactable GitHub node.
// The reaction parameter accepts CLI-friendly names (e.g. "thumbs_up").
func React(api ghcli.API, subjectID, reaction string) error {
	graphqlContent, ok := ValidReactions[reaction]
	if !ok {
		return fmt.Errorf("invalid reaction %q", reaction)
	}
	return ReactRaw(api, subjectID, graphqlContent)
}

// ReactRaw adds a reaction using the raw GraphQL ReactionContent enum value
// (e.g. "THUMBS_UP"). Use this when the caller has already mapped the value.
func ReactRaw(api ghcli.API, subjectID, graphqlContent string) error {
	variables := map[string]interface{}{
		"subjectId": subjectID,
		"content":   graphqlContent,
	}
	return api.GraphQL(addReactionMutation, variables, nil)
}
