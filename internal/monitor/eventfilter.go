package monitor

import (
	"fmt"
	"strings"

	"github.com/elecnix/gh-monitor/internal/prefs"
)

// EventFilter is a per-event-kind allowlist for the Run/Once emit path. A
// non-nil filter suppresses (drops) any Notification whose Type is not in the
// allowlist; a nil filter emits everything (today's behaviour, the default).
//
// The filter matches the Type string on the Notification, which is either an
// EventType string (e.g. "new-failing-checks") or one of the loop-level
// kinds that are not Diff events but do have templates: "first-poll" and
// "all-clear". Matching is exact and case-sensitive after normalisation; the
// parser lower-cases its input so callers can pass either case.
//
// Design note: the filter lives at the emit boundary rather than inside the
// Diff functions. Diff is a pure change-detector with no notion of
// "should this be surfaced", and several kinds (first-poll, ci-all-green on
// first poll, terminal merged/closed on first poll) are emitted by the loop
// itself rather than produced by Diff. Filtering at emit is the single
// chokepoint every notification flows through, so one guard covers every kind.
type EventFilter struct {
	allowed map[string]bool
}

// validEventKinds is the complete set of notification Type strings the loop
// can emit. It is the union of the prefs template keys (the documented,
// user-facing kind list, which already includes every EventType plus the two
// loop-level kinds first-poll and all-clear) with the EventType constants —
// the latter is a defensive superset so a future EventType added without a
// matching template entry is still a recognised filter kind. This is the
// authoritative allowlist for ParseEventFilter validation so a typo fails
// loudly instead of silently muting the kind the caller wanted.
func validEventKinds() map[string]bool {
	out := make(map[string]bool, 32)
	for _, k := range prefs.TemplateKeys() {
		out[k] = true
	}
	// Defensive: every EventType constant, in case one is added without a
	// template entry. Today these are all already covered by TemplateKeys.
	for _, e := range []EventType{
		EventNewFailingChecks, EventCIAllGreen, EventNewUnresolvedThreads,
		EventNewGeneralComments, EventConflict, EventReviewApproved,
		EventReviewChangesRequested, EventReviewDismissed, EventNewCommit,
		EventMerged, EventClosed, EventIssueClosed, EventIssueReopened,
		EventIssueNewComment, EventIssueMention, EventRunQueued,
		EventRunInProgress, EventRunCompleted, EventRepoNewPR, EventRepoNewIssue,
		EventRepoReadiness, EventDegraded,
	} {
		out[string(e)] = true
	}
	return out
}

// NewEventFilter builds an allowlist from the given event-kind strings. With
// no arguments it returns a non-nil filter that suppresses everything (the
// "mute all" case); pass nil to RunOptions.EventFilter (or leave it unset) to
// emit everything. The kinds are normalised (trimmed, lower-cased) but NOT
// validated against the known set — use ParseEventFilter for caller-facing
// input that must reject typos.
func NewEventFilter(kinds ...string) *EventFilter {
	f := &EventFilter{allowed: make(map[string]bool, len(kinds))}
	for _, k := range kinds {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			f.allowed[k] = true
		}
	}
	return f
}

// ParseEventFilter parses a comma-separated list of event kinds into an
// EventFilter, rejecting any kind that is not a recognised notification type.
// Empty/blank entries are dropped, surrounding whitespace is trimmed, and
// matching is case-insensitive. An empty input string returns a non-nil filter
// that suppresses everything (callers wanting "emit everything" should pass a
// nil EventFilter instead, i.e. leave the option unset).
func ParseEventFilter(s string) (*EventFilter, error) {
	valid := validEventKinds()
	f := &EventFilter{allowed: make(map[string]bool)}
	for _, raw := range strings.Split(s, ",") {
		k := strings.ToLower(strings.TrimSpace(raw))
		if k == "" {
			continue
		}
		if !valid[k] {
			return nil, fmt.Errorf("unknown event kind %q (expected one of the notification types)", k)
		}
		f.allowed[k] = true
	}
	return f, nil
}

// filterEmit wraps a caller's emit callback with the EventFilter. A nil
// filter (or nil emit) returns a safe no-op so the default path is
// zero-overhead and a nil emit never panics. A non-nil filter drops any
// Notification whose Type is not allowed before it reaches emit.
func filterEmit(f *EventFilter, emit func(Notification)) func(Notification) {
	if emit == nil {
		return func(Notification) {} // no-op: nothing to call
	}
	if f == nil {
		return emit // nil filter: pass everything through unchanged
	}
	return func(n Notification) {
		if f.Allows(n.Type) {
			emit(n)
		}
	}
}

// Allows reports whether a notification Type passes the filter. A nil filter
// allows everything; a non-nil filter allows only the kinds in its allowlist.
func (f *EventFilter) Allows(typ string) bool {
	if f == nil {
		return true
	}
	return f.allowed[strings.ToLower(strings.TrimSpace(typ))]
}

// String renders the allowlist as a sorted, comma-separated list for logs and
// the --help text. Returns "<all>" for a nil filter and "<none>" for an empty
// allowlist so the two are distinguishable in diagnostics.
func (f *EventFilter) String() string {
	if f == nil {
		return "<all>"
	}
	if len(f.allowed) == 0 {
		return "<none>"
	}
	out := make([]string, 0, len(f.allowed))
	for k := range f.allowed {
		out = append(out, k)
	}
	// stable order for logs
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return strings.Join(out, ",")
}
