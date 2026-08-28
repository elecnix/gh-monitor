package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/elecnix/gh-monitor/backend"
)

// ackReaction is the reaction added to a comment when its notification is
// delivered: 👀 tells humans on the PR that an agent received it. It is
// evidence of delivery, not of action — the loop-breaker remains the 👍 the
// consumer adds after acting.
const ackReaction = "eyes"

// notifier acknowledges one reactable GitHub node with a reaction. It is the
// seam the monitor loop's eyes-on-notify hook calls through; the production
// implementation delegates to the registry's ReactionActor.
type notifier interface {
	Ack(nodeID, reaction string) error
}

// reactionNotifier adapts a backend Registry's ReactionsFor capability to the
// notifier seam. The actor is resolved lazily per call so tests can stub the
// API underneath it.
type reactionNotifier struct {
	reactFn func() (backend.ReactionActor, error)
	target  backend.Target
}

func (r *reactionNotifier) Ack(nodeID, reaction string) error {
	actor, err := r.reactFn()
	if err != nil {
		return err
	}
	return actor.React(context.TODO(), r.target, nodeID, reaction)
}

// ackNodeIDs returns the reactable comment node IDs a delivered update is
// about: the thread comments and general comments for PR events, and the
// issue comments for issue events. Every other kind yields nothing — a
// failing check or a merged PR has no comment to acknowledge.
func ackNodeIDs(ev backend.Event) []string {
	var ids []string
	switch ev.Type {
	case backend.EventNewUnresolvedThreads:
		for _, t := range ev.Threads {
			// The thread's FIRST comment is the anchor a 👀 belongs on: it is
			// the review point a human reads, and the ID the thread-claiming
			// convention in the skill already uses.
			if len(t.CommentIDs) > 0 {
				ids = append(ids, t.CommentIDs[0])
			}
		}
	case backend.EventNewGeneralComments:
		for _, c := range ev.Comments {
			ids = append(ids, c.ID)
		}
	case backend.EventIssueNewComment, backend.EventIssueMention:
		for _, c := range ev.IssueComments {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// ackOnDeliver reacts to every comment a delivered update is about: one
// reaction per node, failures logged not fatal. A failure costs one stderr
// line and the watch continues — a degraded ack beats a lost notification.
func ackOnDeliver(n notifier, ev backend.Event, errOut io.Writer) {
	for _, id := range ackNodeIDs(ev) {
		if err := n.Ack(id, ackReaction); err != nil {
			_, _ = fmt.Fprintf(errOut,
				"gh-monitor: eyes-on-notify reaction failed for %s (%v); continuing\n", id, err)
		}
	}
}
