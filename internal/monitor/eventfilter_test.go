package monitor

import (
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventFilter_Allows is the core RED→GREEN test for the per-event-kind
// allowlist. It exercises EventFilter.Allows directly (the predicate the
// emit boundary uses to drop suppressed notifications).
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

// TestEventFilter_DegradedIsARecognisedKind confirms degraded is a first-class
// notification kind (issue #66): it parses, it can be allowlisted, and it is
// rejected like any other typo-adjacent spelling.
func TestEventFilter_DegradedIsARecognisedKind(t *testing.T) {
	f, err := ParseEventFilter("degraded")
	require.NoError(t, err, "--events degraded must be accepted")
	assert.True(t, f.Allows("degraded"))
	assert.False(t, f.Allows("merged"))

	f, err = ParseEventFilter("merged,DEGRADED")
	require.NoError(t, err)
	assert.True(t, f.Allows("degraded"), "matching is case-insensitive")
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

// TestEventFilter_EveryBackendEventTypeIsARecognisedKind replaces the
// hand-maintained list of EventType constants that used to sit in
// validEventKinds. That list claimed to be a superset of the constants but
// named 22 of the 25 backend declares; it was only correct because
// prefs.TemplateKeys happened to cover the three it omitted.
//
// The rule now is: anything backend.AllEventTypes reports is a kind --events
// accepts, whether or not it has a template entry. The loop-level kinds the
// backend does not emit (first-poll, all-clear) and the client's own "timeout"
// kind are asserted separately, because those are exactly the ones a
// vocabulary read from backend must not be expected to carry.
func TestEventFilter_EveryBackendEventTypeIsARecognisedKind(t *testing.T) {
	for _, e := range backend.AllEventTypes() {
		f, err := ParseEventFilter(string(e))
		require.NoErrorf(t, err, "--events %s must be accepted", e)
		assert.Truef(t, f.Allows(string(e)), "the filter must allow the kind it just accepted")
		assert.Falsef(t, f.Allows(string(e)+"-nope"), "%s must not match a longer name", e)
	}
}

// TestEventFilter_LoopLevelKindsAreRecognised covers the kinds --events accepts
// that are not backend events: the two the loop emits itself, and the client's
// own timeout line (issue #127).
func TestEventFilter_LoopLevelKindsAreRecognised(t *testing.T) {
	for _, kind := range []string{"first-poll", "all-clear", "timeout"} {
		f, err := ParseEventFilter(kind)
		require.NoErrorf(t, err, "--events %s must be accepted", kind)
		assert.True(t, f.Allows(kind))
	}
}

// TestEventFilter_RejectsSomethingOutsideTheVocabulary guards the other
// direction: reading the vocabulary from backend must not make the filter
// accept everything. A typo still fails loudly rather than silently muting the
// kind the caller wanted.
func TestEventFilter_RejectsSomethingOutsideTheVocabulary(t *testing.T) {
	for _, typo := range []string{"not-a-real-kind", "check-annotationss", "issues", "thread"} {
		_, err := ParseEventFilter(typo)
		assert.Errorf(t, err, "--events %s must be rejected", typo)
	}
}
