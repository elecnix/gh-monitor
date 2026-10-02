package monitor

import (
	"fmt"
	"strings"

	"github.com/elecnix/gh-monitor/backend"
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
// loop-level kinds first-poll and all-clear) with every EventType backend
// declares. The latter is a defensive superset so a future EventType added
// without a matching template entry is still a recognised filter kind; it is
// read from backend.AllEventTypes rather than copied out by hand, so a new
// EventType is covered the day it is declared. This is the authoritative
// allowlist for ParseEventFilter validation so a typo fails loudly instead of
// silently muting the kind the caller wanted.
func validEventKinds() map[string]bool {
	out := make(map[string]bool, 32)
	for _, k := range prefs.TemplateKeys() {
		out[k] = true
	}
	for _, e := range backend.AllEventTypes() {
		out[string(e)] = true
	}
	// The client's own timeout line (issue #127) is a loop-level kind like
	// first-poll and all-clear: the renderer never receives it as a backend
	// event, but --events may name it so a bounded caller can route it.
	out["timeout"] = true
	return out
}

// ParseEventFilter parses a comma-separated list of event kinds into an
// EventFilter, rejecting any kind that is not a recognised notification type.
// This is the only constructor: every caller-facing kind string goes through
// the validation, so a typo fails loudly instead of silently muting the kind
// the caller wanted. Empty/blank entries are dropped, surrounding whitespace
// is trimmed, and matching is case-insensitive. An empty input string returns
// a non-nil filter that suppresses everything (callers wanting "emit
// everything" should pass a nil EventFilter instead, i.e. leave the option
// unset).
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
