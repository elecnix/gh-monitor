package monitor

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"
)

// BudgetGuard provides advisory awareness of the shared GitHub GraphQL
// budget. GitHub resets the GraphQL quota hourly; when many watchers consume
// it continuously, it exhausts before the reset and stays exhausted because
// consumption is continuous. The guard reads GET /rate_limit (REST, cheap)
// and lets a caller stretch its cadence as the GraphQL budget runs low, so
// the watcher contributes less instead of failing wholesale at the boundary.
//
// Advisory by design: /rate_limit cannot see secondary rate limits (GitHub
// throttles request rate and concurrency separately from the published
// quota), so a 403 body remains authoritative. The guard only slows cadence;
// it never assumes a healthy-looking quota means a request will succeed.
// Rate-limit errors keep their existing hard backoff path.
type BudgetGuard struct {
	svc *Service

	// checkEvery bounds how often the guard re-reads rate_limit. Each read is
	// one REST call, so refreshing at most every 30s keeps the advisory
	// overhead trivial even for a 10s-interval watcher.
	checkEvery time.Duration

	// base is the watcher's normal poll interval; the stretch is derived from
	// it so a fast watcher slows proportionally.
	base time.Duration

	// thresholdFraction is the fraction of the GraphQL limit below which the
	// budget counts as low (default 10%: stretch when under a tenth remains).
	thresholdFraction float64

	// maxStretch caps the extra delay a single stretch can add (default
	// maxIdleInterval, matching the idle-backoff ceiling).
	maxStretch time.Duration

	mu        sync.Mutex
	lastCheck time.Time
	cached    *RateLimitResponse
	wasLow    bool // last known low state, for transition notices
}

// NewBudgetGuard builds a guard reading rate_limit through svc. base is the
// watcher's poll interval (used to scale the stretch); nil svc yields a guard
// that never stretches (safe for tests that do not exercise the budget).
func NewBudgetGuard(svc *Service, base time.Duration) *BudgetGuard {
	g := &BudgetGuard{
		svc:               svc,
		checkEvery:        30 * time.Second,
		base:              base,
		thresholdFraction: 0.10,
		maxStretch:        maxIdleInterval,
	}
	if g.base <= 0 {
		g.base = defaultInterval
	}
	return g
}

// BudgetState is the result of one Stretch call.
type BudgetState struct {
	// Remaining and Limit are the last-known GraphQL budget values (zero when
	// the rate-limit endpoint could not be read).
	Remaining int
	Limit     int

	// Low is true when the advisory budget is below the threshold.
	Low bool

	// Extra is the delay to add to the next poll when Low (zero otherwise).
	Extra time.Duration

	// Changed is true when Low differs from the previous Stretch — a
	// transition into or out of the low state. Callers emit a loud notice on
	// transitions so a stretched cadence is never silently applied or
	// silently lifted.
	Changed bool
}

// Stretch returns the advisory delay adjustment for the next poll. It
// refreshes the cached rate limit at most once per checkEvery; between
// refreshes it answers from the cache. When the rate-limit endpoint itself
// cannot be read, it returns a no-op state (Low=false, Extra=0) — a blind
// guard must not guess.
func (g *BudgetGuard) Stretch(now time.Time) BudgetState {
	st := BudgetState{}
	if g == nil || g.svc == nil {
		return st
	}
	remaining, limit, ok := g.GraphQLRemaining(now)
	if !ok {
		return st
	}
	st.Remaining = remaining
	st.Limit = limit

	threshold := int(float64(limit) * g.thresholdFraction)
	if limit <= 0 {
		return st
	}
	if remaining >= threshold {
		st.Low = false
	} else {
		st.Low = true
		deficit := float64(threshold-remaining) / float64(threshold)
		if deficit > 1 {
			deficit = 1
		}
		st.Extra = time.Duration(float64(g.base) * 3 * deficit)
		if st.Extra > g.maxStretch {
			st.Extra = g.maxStretch
		}
	}

	g.mu.Lock()
	st.Changed = st.Low != g.wasLow
	g.wasLow = st.Low
	g.mu.Unlock()
	return st
}

// GraphQLRemaining returns the last-known GraphQL remaining/limit, refreshing
// from GET /rate_limit at most once per checkEvery. ok=false when the
// rate-limit endpoint cannot be read (e.g. REST also degraded).
func (g *BudgetGuard) GraphQLRemaining(now time.Time) (remaining, limit int, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastCheck.IsZero() || now.Sub(g.lastCheck) >= g.checkEvery {
		rl, err := g.svc.FetchRateLimit()
		if err == nil && rl != nil {
			g.cached = rl
			g.lastCheck = now
		}
	}
	if g.cached == nil {
		return 0, 0, false
	}
	return g.cached.Resources.GraphQL.Remaining, g.cached.Resources.GraphQL.Limit, true
}

// currentTier returns the fetch tier for the next poll: TierFull when no
// BudgetGuard is wired, otherwise derived from the advisory GraphQL budget.
func (o *RunOptions) currentTier() QueryTier {
	if o.Budget == nil {
		return TierFull
	}
	if remaining, limit, ok := o.Budget.GraphQLRemaining(o.now()); ok {
		return TierForRemaining(remaining, limit)
	}
	return TierFull
}

// emitTierNotice emits a loud degraded notification when the fetch tier drops
// (naming exactly what is no longer being watched) or recovers. A watcher
// that quietly sheds surfaces would turn a missing signal into an apparent
// all-clear; this is the loud half of that rule.
func emitTierNotice(opts RunOptions, tier QueryTier, emit func(Notification)) {
	label := degradedLabel(opts)
	var msg string
	if shed := tier.ShedSurfaces(); len(shed) > 0 {
		msg = fmt.Sprintf(
			"⚠️ GraphQL budget low on %s: no longer watching %s until the budget recovers",
			label, strings.Join(shed, ", "))
	} else {
		msg = fmt.Sprintf("✅ GraphQL budget recovered on %s: resuming full monitoring", label)
	}
	emit(Notification{
		Type:      string(EventDegraded),
		PRLabel:   label,
		Message:   msg,
		Timestamp: opts.now(),
	})
}

// isQueryCostError reports whether a GraphQL error is a per-query resource
// limit ("Resource limits for this query exceeded") rather than a rate-limit
// 403. A cheaper tier can pass where the richer query failed; a rate-limit
// 403 cannot — every query costs points, so it keeps the hard backoff.
func IsQueryCostError(err error) bool {
	var gqlErr *ghcli.GraphQLError
	if errors.As(err, &gqlErr) {
		msg := strings.ToLower(gqlErr.Error())
		return strings.Contains(msg, "resource limits") ||
			(strings.Contains(msg, "query") && strings.Contains(msg, "exceeded"))
	}
	return false
}

// applyBudgetStretch consults the loop's BudgetGuard (when set), emits a loud
// notice on transitions into/out of the low state, and returns the delay with
// any stretch added, capped at the idle-backoff ceiling.
func applyBudgetStretch(opts RunOptions, d time.Duration, emit func(Notification)) time.Duration {
	if opts.Budget == nil {
		return d
	}
	st := opts.Budget.Stretch(opts.now())
	if st.Changed {
		emitBudgetNotice(opts, st, emit)
	}
	d += st.Extra
	if d > maxIdleInterval {
		d = maxIdleInterval
	}
	return d
}

// emitBudgetNotice emits a degraded-type notification on a budget transition:
// entering the low state says the cadence is being stretched (loud, so a
// slow-down is never silent); leaving it says normal cadence resumes.
func emitBudgetNotice(opts RunOptions, st BudgetState, emit func(Notification)) {
	label := degradedLabel(opts)
	var msg string
	if st.Low {
		msg = fmt.Sprintf(
			"⚠️ GraphQL budget low (%d/%d) on %s: stretching poll interval to slow consumption until the reset",
			st.Remaining, st.Limit, label)
	} else {
		msg = fmt.Sprintf("✅ GraphQL budget recovered on %s: resuming normal poll cadence", label)
	}
	emit(Notification{
		Type:      string(EventDegraded),
		PRLabel:   label,
		Message:   msg,
		Timestamp: opts.now(),
	})
}
