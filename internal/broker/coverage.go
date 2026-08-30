package broker

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Coverage tracks which repositories the broker wake path actually answers
// for, so a caller can decide per repository — never globally — whether
// skipping a scheduled poll is safe.
//
// The distinction matters because a broker connection is one socket carrying
// many repositories' events, and being connected says nothing about which
// repositories publish to it. A daemon that read "transport healthy" as
// "every watched repository is covered" would suppress polling for a repo no
// webhook exists on, and that repo would go silent — precisely the failure
// mode this package's doctrine ("absence is not success") exists to prevent,
// reintroduced one layer up.
//
// Coverage is therefore built from positive evidence only: a repository is
// covered once an event for it has actually been delivered here, and stays
// covered until a full TTL passes with no further event. That decay is not a
// cache detail, it is the second half of the guarantee — a webhook removed
// from a repository stops producing evidence, and coverage built on that
// evidence must expire rather than mute the repository forever.
//
// A declared subscription filter (Config.Topic) is deliberately not consulted.
// It can only ever say which events the broker is willing to forward, never
// whether any are being produced, so intersecting it with observed evidence
// could only discard evidence that already proved delivery.
//
// The zero value is not usable; call NewCoverage. A nil *Coverage is usable
// and covers nothing, so a caller with no broker configured needs no special
// case.
type Coverage struct {
	ttl time.Duration

	// now is the clock, a field so tests can drive TTL decay without sleeping.
	now func() time.Time

	mu   sync.RWMutex
	seen map[string]time.Time
}

// envCoverageTTL names the environment variable CoverageTTLFromEnv reads.
const envCoverageTTL = "GH_MONITOR_BROKER_COVERAGE_TTL"

// DefaultCoverageTTL is how long one delivered event vouches for its
// repository. It is deliberately long relative to any poll interval: the
// question it answers is "does this repository deliver events here at all",
// which a whole workday of quiet on an active repo should not flip. It is
// also deliberately finite, so a webhook that is removed stops muting its
// repository within a working day rather than never.
const DefaultCoverageTTL = 6 * time.Hour

// NewCoverage returns an empty Coverage whose observations expire after ttl.
// A non-positive ttl covers nothing at all: a misconfigured decay window must
// fail towards polling, never towards silence.
func NewCoverage(ttl time.Duration) *Coverage {
	return &Coverage{ttl: ttl, now: time.Now, seen: make(map[string]time.Time)}
}

// CoverageTTLFromEnv reads the coverage TTL from GH_MONITOR_BROKER_COVERAGE_TTL
// (seconds), falling back to def when unset, unparsable, or non-positive —
// the same shape as IdleCapFromEnv, and for the same reason: a bad value must
// not silently change how much polling is suppressed.
func CoverageTTLFromEnv(def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envCoverageTTL))
	if raw == "" {
		return def
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return def
	}
	return time.Duration(secs) * time.Second
}

// Note records that an event for owner/repo was delivered here. It reports
// true only when this establishes coverage the repository did not already
// have — a first event, or the first after a lapse — so a caller can log one
// line per repository instead of one per event.
func (c *Coverage) Note(owner, repo string) bool {
	if c == nil || c.ttl <= 0 {
		// Coverage is disabled: nothing can become covered, so there is
		// nothing to record and nothing to announce.
		return false
	}
	key := coverageKey(owner, repo)
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.seen[key]
	c.seen[key] = now
	if len(c.seen) > pruneAbove {
		c.pruneLocked(now)
	}
	return !ok || now.Sub(last) >= c.ttl
}

// pruneAbove bounds the observation map, which is fed by whatever
// repositories the broker delivers — a wildcard subscription on a busy
// organisation would otherwise accumulate an entry per repository forever,
// including ones that stopped publishing months ago. The threshold is far
// above any realistic working set, so pruning is rare and never on the hot
// path of a daemon watching a handful of repositories.
const pruneAbove = 1024

// pruneLocked drops observations that have already lapsed. Called with c.mu
// held. Dropping a lapsed entry is not a behaviour change: Covers already
// reports false for it, and a later event simply re-establishes coverage.
func (c *Coverage) pruneLocked(now time.Time) {
	for k, seen := range c.seen {
		if now.Sub(seen) >= c.ttl {
			delete(c.seen, k)
		}
	}
}

// Covers reports whether owner/repo has delivered an event within the TTL.
// A repository that never has — or has gone a full TTL without one — is not
// covered, and its caller must keep polling it.
func (c *Coverage) Covers(owner, repo string) bool {
	if c == nil || c.ttl <= 0 {
		return false
	}
	c.mu.RLock()
	last, ok := c.seen[coverageKey(owner, repo)]
	c.mu.RUnlock()
	return ok && c.now().Sub(last) < c.ttl
}

// coverageKey normalises owner/repo the way GitHub treats them: case
// -insensitively. Events and watches routinely disagree on casing, and a
// case-sensitive key would leave a watch polling forever while its own
// events arrived under a different spelling.
func coverageKey(owner, repo string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo)
}
