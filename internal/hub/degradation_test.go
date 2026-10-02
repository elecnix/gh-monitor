package hub

import (
	"errors"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below drive the seam directly. Before the extraction this logic
// was only reachable by standing up a whole Hub, a poller goroutine and a
// channel with a timeout — three lines of accounting behind all of that
// scaffolding. The invariants they pin are load-bearing: the blind-window
// semantics were fixed three times (#99, #102, #103) and every fix is an
// assertion here.

// TestDegradation_RepeatingFailureAnnouncesOnce pins the episode contract of
// issue #66: consecutive identical failed fetches are one episode, one
// broadcast. A poller that re-announces every failed poll turns a single
// outage into an unbounded stream of identical notices.
func TestDegradation_RepeatingFailureAnnouncesOnce(t *testing.T) {
	d := newDegradation(backend.KindPR)
	err := errors.New("gh api failed: exit status 1")

	ev, announce := d.fail("graphql", err)
	require.True(t, announce, "the first failure of an episode is new information")
	assert.Equal(t, "graphql", ev.DegradedSurface)
	assert.Equal(t, err.Error(), ev.DegradedMessage)

	_, announce = d.fail("graphql", err)
	assert.False(t, announce, "an identical repeat failure is the same episode and must not re-announce")
}

// TestDegradation_ChangedMessageAnnouncesAgain is the other half of #66: a
// different error on the same surface IS new information, because the previous
// notice told subscribers something that is no longer true.
func TestDegradation_ChangedMessageAnnouncesAgain(t *testing.T) {
	d := newDegradation(backend.KindPR)

	_, announce := d.fail("graphql", errors.New("first"))
	require.True(t, announce)

	_, announce = d.fail("graphql", errors.New("second"))
	assert.True(t, announce, "a changed error on a degraded surface must re-announce")
}

// TestDegradation_WindowOpensAtLastObservationNotAtFailure is the unit form
// of TestPoller_BlindWindowStartsAtLastSuccess: the window opens at the last
// successful observation, never at the failure that discovers the blindness.
// The poller-level test proves it end to end; this one proves the rule lives
// in the accounting and not in a caller's ordering, so a future refactor
// cannot reintroduce the two-line dance at a new call site.
func TestDegradation_WindowOpensAtLastObservationNotAtFailure(t *testing.T) {
	d := newDegradation(backend.KindPR)

	observedAt := time.Now().Truncate(time.Second)
	d.success("o/r#1", observedAt)
	assert.Empty(t, d.success("o/r#1", time.Now()),
		"a success with no episode in flight announces nothing")

	_, announce := d.fail("graphql", errors.New("boom"))
	require.True(t, announce)

	// Recover five seconds later: the declared window must span the outage, not
	// collapse onto it.
	notices := d.success("o/r#1", observedAt.Add(5*time.Second))
	require.Len(t, notices, 1, "one degraded surface, one recovery notice")

	from, err := time.Parse(time.RFC3339, notices[0].DegradedFrom)
	require.NoError(t, err, "DegradedFrom must be RFC 3339")
	to, err := time.Parse(time.RFC3339, notices[0].DegradedTo)
	require.NoError(t, err, "DegradedTo must be RFC 3339")

	assert.WithinDuration(t, observedAt, from, time.Second,
		"DegradedFrom must be the last successful observation, not the failure")
	assert.GreaterOrEqual(t, to.Sub(from), 5*time.Second,
		"the declared window must span the outage, not collapse onto it")
	assert.Contains(t, notices[0].Notice, "not observed",
		"the recovery must declare the gap: absence is not success")
	assert.Contains(t, notices[0].Notice, "recovered", "the recovery must say so")
}

// TestDegradation_NoPriorObservationDeclaresNoWindow covers the honest
// unknowable case (issue #99): a poller whose very first fetch failed has no
// last successful observation, so the window's start is unknown. Declaring no
// interval is the truthful answer; stamping the failure time would be a
// precise-looking lie about what was and was not observed.
func TestDegradation_NoPriorObservationDeclaresNoWindow(t *testing.T) {
	d := newDegradation(backend.KindPR)

	_, announce := d.fail("graphql", errors.New("first fetch failed"))
	require.True(t, announce, "even with no prior success the failure must be announced")

	notices := d.success("o/r#1", time.Now())
	require.Len(t, notices, 1)
	assert.Empty(t, notices[0].DegradedFrom,
		"with no prior successful observation the window start is unknowable: declare no interval")
	assert.Empty(t, notices[0].DegradedTo,
		"no window declared means no window end either")
}

// TestDegradation_LastObservationAdvancesOnlyOnSuccess pins which timestamp
// the next window opens at: only a success may advance it. If a failure
// advanced lastOK, the window would open at the failure and shrink by exactly
// the blind interval — the #103 bug, arriving through a different door.
func TestDegradation_LastObservationAdvancesOnlyOnSuccess(t *testing.T) {
	d := newDegradation(backend.KindPR)

	firstObservation := time.Now().Truncate(time.Second)
	d.success("o/r#1", firstObservation)

	// A long failure episode. Failures must not move lastOK.
	for i := 0; i < 3; i++ {
		_, _ = d.fail("graphql", errors.New("boom"))
	}
	secondObservation := time.Now().Truncate(time.Second)
	d.success("o/r#1", secondObservation)

	_, announce := d.fail("graphql", errors.New("boom again"))
	require.True(t, announce)
	notices := d.success("o/r#1", time.Now())
	require.Len(t, notices, 1)

	from, err := time.Parse(time.RFC3339, notices[0].DegradedFrom)
	require.NoError(t, err)
	assert.WithinDuration(t, secondObservation, from, time.Second,
		"the second window opens at the second observation, not the first and not the failure")
}

// TestDegradation_MultiSurfaceRecoveryAnnouncesEverySurfaceSorted covers the
// two-surface shape — GraphQL fails, then the REST fallback fails too — where
// one fetch result leaves two open episodes. Each must be named, and the order
// must be deterministic so a log diff of two runs lines up.
func TestDegradation_MultiSurfaceRecoveryAnnouncesEverySurfaceSorted(t *testing.T) {
	d := newDegradation(backend.KindPR)
	d.success("o/r#1", time.Now())

	_, first := d.fail("graphql", errors.New("graphql down"))
	require.True(t, first)
	_, second := d.fail("rest", errors.New("rest read failed"))
	require.True(t, second)

	notices := d.success("o/r#1", time.Now())
	require.Len(t, notices, 2, "every degraded surface recovers separately")
	assert.Contains(t, notices[0].Notice, "(graphql)")
	assert.Contains(t, notices[1].Notice, "(rest)")
	assert.Less(t, notices[0].Notice, notices[1].Notice, "recovery notices are emitted in sorted surface order")

	// The episodes are closed: a second success announces nothing.
	assert.Empty(t, d.success("o/r#1", time.Now()),
		"the episode ended with the first recovery; a later clean poll is not news")
}

// TestDegradation_FailureNoticeNamesTheBlindSurfaces pins that the degraded
// notice carries the watched-surface guarantees the failed fetch stops
// delivering (issue #98). A notice naming only the API ("graphql") would let a
// caller keep trusting CI signals the degraded query can no longer deliver.
func TestDegradation_FailureNoticeNamesTheBlindSurfaces(t *testing.T) {
	d := newDegradation(backend.KindPR)
	ev, announce := d.fail("graphql", errors.New("boom"))
	require.True(t, announce)
	assert.Equal(t, []string{"check outcomes", "head commit", "mergeability"}, ev.DegradedSurfaces)

	assert.Equal(t, []string{"check outcomes"}, blindSurfaces(backend.KindCommit))
	assert.Equal(t, []string{"issue state", "comments"}, blindSurfaces(backend.KindIssue))
	assert.Equal(t, []string{"run status"}, blindSurfaces(backend.KindRun))
	assert.Equal(t, []string{"new PRs and issues"}, blindSurfaces(backend.KindRepo))
	// An unrecognised kind is the backend's business to describe, not ours.
	assert.Nil(t, blindSurfaces(backend.Kind("backend-transport")))
}

// TestDegradation_ConcurrentFailAndRecoverHoldsTheInvariant exercises the
// lock under -race: two pollers' worth of concurrent traffic on one ledger
// must never lose a surface or declare a window that starts after it ends.
func TestDegradation_ConcurrentFailAndRecoverHoldsTheInvariant(t *testing.T) {
	d := newDegradation(backend.KindPR)
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				d.fail("graphql", errors.New("boom"))
				for _, ev := range d.success("o/r#1", time.Now()) {
					if ev.DegradedFrom != "" && ev.DegradedTo != "" {
						from, err1 := time.Parse(time.RFC3339, ev.DegradedFrom)
						to, err2 := time.Parse(time.RFC3339, ev.DegradedTo)
						if err1 == nil && err2 == nil && to.Before(from) {
							t.Errorf("declared a window that ends before it starts: %s .. %s",
								ev.DegradedFrom, ev.DegradedTo)
							return
						}
					}
				}
			}
		}()
	}
	for w := 0; w < 4; w++ {
		<-done
	}
}

