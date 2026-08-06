package monitor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventFilter_Allows is the core RED→GREEN test for the per-event-kind
// allowlist. It exercises EventFilter.Allows directly (the predicate the
// emit boundary uses to drop suppressed notifications) plus filterEmit (the
// wrapper Run/Once install around the caller's emit callback).
//
// Cases:
//   - nil filter allows everything (today's default behaviour).
//   - a non-nil allowlist allows only the listed kinds and drops the rest.
//   - an empty (non-nil) allowlist drops everything — the "mute all" case,
//     distinct from nil.
//   - matching is case-insensitive and trims whitespace.
func TestEventFilter_Allows(t *testing.T) {
	// nil filter = emit everything (default).
	var nilFilter *EventFilter
	assert.True(t, nilFilter.Allows("new-failing-checks"))
	assert.True(t, nilFilter.Allows("first-poll"))
	assert.True(t, nilFilter.Allows("anything-at-all"))

	// non-nil allowlist: only listed kinds pass.
	f := NewEventFilter("new-failing-checks", "merged")
	assert.True(t, f.Allows("new-failing-checks"))
	assert.True(t, f.Allows("merged"))
	assert.False(t, f.Allows("first-poll"))
	assert.False(t, f.Allows("new-general-comments"))
	assert.False(t, f.Allows(""))

	// empty allowlist = mute everything (distinct from nil).
	empty := NewEventFilter()
	assert.False(t, empty.Allows("new-failing-checks"))
	assert.False(t, empty.Allows("first-poll"))

	// case-insensitive + whitespace-trimmed.
	mixed := NewEventFilter("  New-Failing-Checks  ")
	assert.True(t, mixed.Allows("new-failing-checks"))
	assert.True(t, mixed.Allows("NEW-FAILING-CHECKS"))
}

// TestEventFilter_FilterEmitDropsSuppressedKinds drives the exact wrapper that
// Run/Once install around the caller's emit callback. It confirms:
//   - a nil filter passes every notification through unchanged;
//   - a non-nil filter emits only allowlisted kinds and silently drops the rest;
//   - a nil emit callback is handled without panic.
func TestEventFilter_FilterEmitDropsSuppressedKinds(t *testing.T) {
	// nil filter: everything passes.
	var got []Notification
	pass := filterEmit(nil, func(n Notification) { got = append(got, n) })
	pass(Notification{Type: "first-poll"})
	pass(Notification{Type: "new-failing-checks"})
	assert.Len(t, got, 2, "nil filter must pass every notification through")

	// non-nil filter: only allowlisted kinds reach emit.
	got = nil
	pass = filterEmit(NewEventFilter("new-failing-checks", "merged"), func(n Notification) { got = append(got, n) })
	pass(Notification{Type: "first-poll"})           // suppressed
	pass(Notification{Type: "new-failing-checks"})   // allowed
	pass(Notification{Type: "new-general-comments"}) // suppressed
	pass(Notification{Type: "merged"})               // allowed
	assert.Equal(t, []string{"new-failing-checks", "merged"}, typesOf(got),
		"only allowlisted kinds must reach emit, in order")

	// nil emit callback must not panic even with a filter.
	require.NotPanics(t, func() {
		noop := filterEmit(NewEventFilter("merged"), nil)
		noop(Notification{Type: "merged"})
	})
}

// TestEventFilter_ParsesCommaList confirms the parser splits a comma-separated
// list, trims whitespace, drops empties, and is case-insensitive.
func TestEventFilter_ParsesCommaList(t *testing.T) {
	f, err := ParseEventFilter(" conflict , new-failing-checks ,, Merged ")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.True(t, f.Allows("conflict"))
	assert.True(t, f.Allows("new-failing-checks"))
	assert.True(t, f.Allows("merged"))
	assert.False(t, f.Allows("new-commit"))
}

// TestEventFilter_RejectsUnknownKind confirms an unknown event kind is an
// error rather than silently ignored (a typo would otherwise mute everything
// the caller actually wanted).
func TestEventFilter_RejectsUnknownKind(t *testing.T) {
	_, err := ParseEventFilter("conflict,not-a-real-kind")
	assert.Error(t, err)
}

// TestEventFilter_EmptyInputSuppressesAll confirms an empty input string to
// ParseEventFilter yields a non-nil filter that suppresses everything (the
// "mute all" case), distinct from a nil EventFilter (emit everything).
func TestEventFilter_EmptyInputSuppressesAll(t *testing.T) {
	f, err := ParseEventFilter("")
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.False(t, f.Allows("first-poll"))
	assert.False(t, f.Allows("merged"))
}

// TestEventFilter_String confirms the diagnostic rendering distinguishes the
// three states: nil (<all>), empty (<none>), and a populated allowlist
// (sorted, comma-separated).
func TestEventFilter_String(t *testing.T) {
	var nilFilter *EventFilter
	assert.Equal(t, "<all>", nilFilter.String())

	empty := NewEventFilter()
	assert.Equal(t, "<none>", empty.String())

	populated := NewEventFilter("merged", "conflict", "new-failing-checks")
	s := populated.String()
	assert.Equal(t, "conflict,merged,new-failing-checks", s, "String must be sorted and comma-separated")
	assert.True(t, strings.Contains(s, "conflict"))
}
