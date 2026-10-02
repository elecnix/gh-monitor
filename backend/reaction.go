package backend

// The reaction vocabulary.
//
// A reaction name is this protocol's own token. It is what `gh monitor react
// --type` accepts, what ReactionActor.React takes, and what crosses the wire
// to an out-of-process backend. The GraphQL ReactionContent enum the built-in
// `gh` backend sends upstream ("THUMBS_UP") is a GitHub API detail, not part
// of this contract — a backend of your own maps the name to whatever its API
// calls it.
//
// These live here rather than beside the CLI for the same reason AllKinds
// does: `internal/` cannot be imported from outside this module, so a table
// kept there is a table no third-party backend can read.
const (
	ReactionThumbsUp   = "thumbs_up"
	ReactionThumbsDown = "thumbs_down"
	ReactionLaugh      = "laugh"
	ReactionHooray     = "hooray"
	ReactionConfused   = "confused"
	ReactionHeart      = "heart"
	ReactionRocket     = "rocket"
	ReactionEyes       = "eyes"
)

// reactionNames is the vocabulary in canonical order, which is sorted order so
// help text and error messages read the same way every time.
var reactionNames = []string{
	ReactionConfused, ReactionEyes, ReactionHeart, ReactionHooray,
	ReactionLaugh, ReactionRocket, ReactionThumbsDown, ReactionThumbsUp,
}

// ReactionNames returns every reaction name the protocol carries, in canonical
// order. The order is stable so `gh monitor react --help` and error messages
// read the same way every time, and the returned slice belongs to the caller.
func ReactionNames() []string {
	out := make([]string, len(reactionNames))
	copy(out, reactionNames)
	return out
}

// ValidReaction reports whether name is a reaction name the protocol carries.
// A server can use it to reject an unknown name itself instead of handing it
// to an API that will not recognise it either.
func ValidReaction(name string) bool {
	for _, n := range reactionNames {
		if n == name {
			return true
		}
	}
	return false
}
