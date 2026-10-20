package hub

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
)

// degradation is one poller's ledger of impaired observation, and the single
// owner of the question a subscriber needs answered on every poll: given this
// poller's history and this fetch result, what notice must they receive?
//
// The invariant it exists to hold is "absence is not success". A fetch that
// stops delivering must be declared while it is failing, and the recovery must
// declare the gap it leaves behind — a cursor advances only on successful
// fetches, so an event missed during the blind window stays missed, and a
// silent recovery would read as an all-clear. That contract has been fixed
// three times (#98, #99, #102, #103), each time by threading one more field
// through one more poller method. Keeping it here means the next fix has one
// place to land.
//
// The poller keeps scheduling — cadence, backoff, jitter, the broker gate —
// and keeps deciding *what* to fetch. degradation owns neither; it owns only
// the promise the fetch made and what it failed to keep.
type degradation struct {
	kind backend.Kind

	mu sync.Mutex
	// surfaces holds the degraded surface -> last announced error message for
	// every episode in flight. It drives the episode contract of issue #66:
	// consecutive identical failed fetches are one episode, one broadcast,
	// while a changed message is new information and re-announces.
	surfaces map[string]string
	// lastOK is when the last successful fetch completed — the honest lower
	// bound for a blind window that opens later. Only success advances it; a
	// failure that advanced it would shrink every window it opened by exactly
	// the blind interval, which is the #103 bug arriving through a new door.
	lastOK time.Time
	// blindFrom is when the open window began: the last successful observation
	// before the first failure of the episode, not the failure itself.
	// time.Now() at the failure would stamp the discovery of the blindness
	// rather than its start, so events between the two would fall outside the
	// declared window and the recovery would claim they were observed. Zero
	// means no observation ever preceded the episode (the very first fetch
	// failed, or a handoff resumed with none yet), so the start is honestly
	// unknowable and the recovery declares no interval at all rather than a
	// precise-looking lie.
	blindFrom time.Time
}

// newDegradation returns the ledger for a poller watching kind. The kind
// selects the watched-surface guarantees a failed fetch stops delivering.
func newDegradation(kind backend.Kind) *degradation {
	return &degradation{kind: kind}
}

// fail records a failed fetch of the given API surface and returns the notice
// to broadcast. announce is false when the failure repeats the last broadcast
// for that surface: consecutive identical failures are one episode, one
// notice, and a poller that re-announces every failed poll turns a single
// outage into an unbounded stream of identical notices (issue #66). A changed
// message does announce — the previous notice told subscribers something that
// is no longer true.
//
// The first failure of an episode opens the blind window at lastOK. The
// notice carries the watched-surface guarantees this target's kind stops
// delivering, not just the API that failed (issue #98): a PR's check outcomes
// ride the same GraphQL query as its comments, so naming only "graphql" would
// let a caller keep trusting CI signals the failed query can no longer deliver.
func (d *degradation) fail(surface string, err error) (ev monitor.Event, announce bool) {
	msg := fmt.Sprintf("%v", err)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.blindFrom.IsZero() {
		d.blindFrom = d.lastOK
	}
	if d.surfaces[surface] == msg {
		return monitor.Event{}, false
	}
	if d.surfaces == nil {
		d.surfaces = make(map[string]string)
	}
	d.surfaces[surface] = msg
	return monitor.Event{
		Type:             monitor.EventDegraded,
		DegradedSurface:  surface,
		DegradedMessage:  msg,
		DegradedSurfaces: blindSurfaces(d.kind),
	}, true
}

// success records a completed fetch and returns the recovery notices — one
// per surface whose episode just ended, in sorted order so a log diff of two
// runs lines up. It returns nil when no episode was in flight: a healthy poll
// is not news.
//
// A recovery is not an all-clear. When a window was open, the notice declares
// its bounds (issue #99): From is the last successful observation before the
// failure, To is this success. With no window — the first fetch failed, or a
// handoff resumed with no observation yet — the notice says only that the
// surface is back, and carries no interval, because the start is unknowable.
func (d *degradation) success(label string, now time.Time) []monitor.Event {
	d.mu.Lock()
	if len(d.surfaces) == 0 {
		d.lastOK = now
		d.mu.Unlock()
		return nil
	}
	out := make([]string, 0, len(d.surfaces))
	for s := range d.surfaces {
		out = append(out, s)
	}
	sort.Strings(out)
	d.surfaces = nil
	d.lastOK = now
	d.mu.Unlock()

	notices := make([]monitor.Event, 0, len(out))
	for _, surface := range out {
		ev := monitor.Event{
			Type:   monitor.EventDegraded,
			Notice: fmt.Sprintf("✅ API recovered (%s) on %s", surface, label),
		}
		// The declaration consumes the window: clearBlindWindow read and
		// cleared, so only the first surface to recover carries the interval.
		// One surface per episode is the normal shape; a GraphQL failure
		// followed by a failed REST fallback is the two-surface one, and that
		// case leaves the second notice without a gap declaration. Preserved
		// as-is here — this is a move, not a re-spec — and flagged for its own
		// issue rather than changed silently underneath one.
		if blindFrom, _ := d.clearBlindWindow(); !blindFrom.IsZero() {
			ev.DegradedFrom = blindFrom.UTC().Format(time.RFC3339)
			ev.DegradedTo = now.UTC().Format(time.RFC3339)
			ev.Notice = fmt.Sprintf(
				"✅ API recovered (%s) on %s — events between %s and %s were not observed and will not be replayed; backfill from REST if completeness matters",
				surface, label, ev.DegradedFrom, ev.DegradedTo)
		}
		notices = append(notices, ev)
	}
	return notices
}

// clearBlindWindow returns and clears the open window's start. ok is false
// with a zero time when no window was open — no degraded episode preceded this
// success, so there is no interval to declare.
func (d *degradation) clearBlindWindow() (blindFrom time.Time, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	blindFrom = d.blindFrom
	d.blindFrom = time.Time{}
	return blindFrom, !blindFrom.IsZero()
}

// blindSurfaces names the watched-surface guarantees a failed fetch of this
// target's kind stops delivering (issue #98). Surfaces on one query are
// coupled: a PR's check outcomes, head commit, and mergeability ride the same
// GraphQL query as its comments and reviews, so a failed PR query suppresses
// check outcomes even though the tier system never sheds them. Naming only the
// failed API ("graphql") would let a caller keep trusting CI signals the
// degraded query can no longer deliver. Ref and commit watches carry check
// outcomes only; issue/run/repo fetches name what their own query carries, and
// a backend transport break names nothing because a backend's surfaces are its
// own to describe.
//
// This is a per-kind table in the same family as kindTraits and labelFor; it
// hangs off no receiver because it reads no state.
func blindSurfaces(kind backend.Kind) []string {
	switch kind {
	case backend.KindPR:
		return []string{"check outcomes", "head commit", "mergeability"}
	case backend.KindRef, backend.KindCommit:
		return []string{"check outcomes"}
	case backend.KindIssue:
		return []string{"issue state", "comments"}
	case backend.KindRun:
		return []string{"run status"}
	case backend.KindRepo:
		return []string{"new PRs and issues"}
	default:
		return nil
	}
}