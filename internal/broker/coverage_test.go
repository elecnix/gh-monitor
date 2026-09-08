package broker

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCoverage_UnobservedRepositoryIsNotCovered is the regression test for the
// bug this type exists to remove: a connected broker used to mean "every
// repository is served by the wake path", so a repo no webhook publishes for
// had its polling suppressed and went silent. Connection health says nothing
// about which repositories reach us; only a delivered event does.
func TestCoverage_UnobservedRepositoryIsNotCovered(t *testing.T) {
	c := NewCoverage(time.Hour)

	assert.False(t, c.Covers("elecnix", "pi-agent-identity"),
		"a repository that has never delivered an event must not be treated as covered")

	// An event for a *different* repository is not evidence about this one.
	c.Note("prizmal-ai", "switch")
	assert.False(t, c.Covers("elecnix", "pi-agent-identity"),
		"an event for another repository must not extend coverage to this one")
	assert.True(t, c.Covers("prizmal-ai", "switch"))
}

// TestCoverage_NoteReportsFirstObservation pins the signal the daemon logs
// off: Note reports true only when it establishes coverage that did not exist
// a moment ago, so an operator gets one line per repository rather than one
// per event.
func TestCoverage_NoteReportsFirstObservation(t *testing.T) {
	c := NewCoverage(time.Hour)

	assert.True(t, c.Note("prizmal-ai", "switch"), "the first event for a repository establishes coverage")
	assert.False(t, c.Note("prizmal-ai", "switch"), "a repeat event must not re-announce established coverage")
}

// TestCoverage_LapsesAfterTTL pins the other half of "absence is not success":
// coverage is evidence with an expiry date. A webhook removed from a repo
// stops delivering events, and coverage derived from those events must decay
// back to "poll it" rather than mute the repo forever on the strength of an
// observation from last week.
func TestCoverage_LapsesAfterTTL(t *testing.T) {
	now := time.Now()
	c := NewCoverage(time.Hour)
	c.now = func() time.Time { return now }

	require.True(t, c.Note("prizmal-ai", "switch"))
	require.True(t, c.Covers("prizmal-ai", "switch"))

	now = now.Add(59 * time.Minute)
	assert.True(t, c.Covers("prizmal-ai", "switch"), "coverage must hold for the whole TTL")

	now = now.Add(2 * time.Minute) // past the hour
	assert.False(t, c.Covers("prizmal-ai", "switch"),
		"coverage must lapse once no event has arrived for a full TTL")

	// A fresh event re-establishes it, and says so.
	assert.True(t, c.Note("prizmal-ai", "switch"), "re-covering a lapsed repository must be announced again")
	assert.True(t, c.Covers("prizmal-ai", "switch"))
}

// TestCoverage_IsCaseInsensitive matches GitHub's own treatment of owner and
// repository names: a webhook delivering "Prizmal-AI/Switch" covers a watch
// resolved as "prizmal-ai/switch". Keying case-sensitively would leave the
// watch polling forever while events for it arrived under a different casing.
func TestCoverage_IsCaseInsensitive(t *testing.T) {
	c := NewCoverage(time.Hour)
	c.Note("Prizmal-AI", "Switch")
	assert.True(t, c.Covers("prizmal-ai", "switch"))
}

// TestCoverage_ZeroTTLCoversNothing guards the degenerate configuration: a
// non-positive TTL must fail closed (keep polling), never open.
func TestCoverage_ZeroTTLCoversNothing(t *testing.T) {
	c := NewCoverage(0)
	assert.False(t, c.Note("prizmal-ai", "switch"),
		"with coverage disabled there is nothing to announce")
	assert.False(t, c.Covers("prizmal-ai", "switch"),
		"a non-positive TTL must suppress nothing rather than cover everything")
}

// TestCoverage_PrunesLapsedObservations keeps the observation map bounded. It
// is fed by whatever repositories the broker delivers — unbounded network
// input — so entries that have already lapsed must not accumulate forever.
func TestCoverage_PrunesLapsedObservations(t *testing.T) {
	now := time.Now()
	c := NewCoverage(time.Hour)
	c.now = func() time.Time { return now }

	for i := range pruneAbove + 1 {
		c.Note("org", fmt.Sprintf("repo-%d", i))
	}
	require.Greater(t, len(c.seen), pruneAbove, "nothing has lapsed yet, so nothing may be dropped")

	// Every observation above is now stale; one fresh event triggers the sweep.
	now = now.Add(2 * time.Hour)
	c.Note("org", "still-active")

	assert.Equal(t, 1, len(c.seen), "lapsed observations must be swept, leaving only the live one")
	assert.True(t, c.Covers("org", "still-active"))
	assert.False(t, c.Covers("org", "repo-0"))
}

// TestCoverage_NilCoversNothing keeps the "no broker wired" path safe: a nil
// *Coverage handed to the hub as an interface value must read as "nothing is
// covered", not panic and not cover everything.
func TestCoverage_NilCoversNothing(t *testing.T) {
	var c *Coverage
	assert.False(t, c.Covers("prizmal-ai", "switch"))
	assert.False(t, c.Note("prizmal-ai", "switch"))
}

func TestCoverageTTLFromEnv(t *testing.T) {
	assert.Equal(t, 6*time.Hour, CoverageTTLFromEnv(6*time.Hour), "unset must fall back to the default")

	t.Setenv(envCoverageTTL, "900")
	assert.Equal(t, 15*time.Minute, CoverageTTLFromEnv(6*time.Hour))

	// A bad or non-positive value must not silently disable coverage decay.
	for _, bad := range []string{"abc", "0", "-1"} {
		t.Setenv(envCoverageTTL, bad)
		assert.Equal(t, 6*time.Hour, CoverageTTLFromEnv(6*time.Hour), "%q must fall back to the default", bad)
	}
}
