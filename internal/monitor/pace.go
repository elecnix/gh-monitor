package monitor

import (
	"sort"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"
)

// PaceConfig holds every constant of the budget pacing calculation.
type PaceConfig struct {
	// Reserve is the fraction of the GraphQL limit no poller plans to spend.
	// It absorbs bursts from other consumers and wakes that bypass pacing.
	Reserve float64

	// MinObserveGap is the shortest span between two observations that
	// yield a consumption rate. A shorter span is too noisy: one response
	// moves the counter by a whole query cost.
	MinObserveGap time.Duration

	// Smoothing is the weight of the newest rate sample in the moving
	// average of other consumers' rate (0 to 1).
	Smoothing float64

	// FullTierCost and CheapTierCost are the measured GraphQL points of one
	// poll at the full tier and at every other tier. The full tier reads
	// annotation counts over REST and fetches bodies only for a check run
	// that changed, so it costs one GraphQL point like the cheaper tiers;
	// the REST read has its own budget and does not enter this one.
	FullTierCost  float64
	CheapTierCost float64
}

// DefaultPaceConfig is the one place the pacing constants live.
var DefaultPaceConfig = PaceConfig{
	Reserve:       0.10,
	MinObserveGap: 10 * time.Second,
	Smoothing:     0.3,
	FullTierCost:  1,
	CheapTierCost: 1,
}

// PollCost estimates the GraphQL points one poll at tier spends.
func (c PaceConfig) PollCost(tier QueryTier) float64 {
	if tier == TierFull {
		return c.FullTierCost
	}
	return c.CheapTierCost
}

// Pacer sizes a poll interval from the shared GraphQL budget.
//
// Let R be the remaining points, L the limit, T the seconds until reset, and
// reserve = Reserve x L. Let o be the points per second other consumers
// spend, N the daemon's pollers and c the mean points per poll. The pacer
// plans for the spend until reset to stay under what is left:
//
//	available = R - reserve - o x T
//	interval  = N x c x T / available
//
// The result is clamped to [base, max(base, T)]: an ample budget leaves the
// base interval unchanged, and no budget left waits for the reset.
//
// The rate o is observed, not configured. Between two observations of the
// same window, the drop in R minus the points this daemon spent (see Spend)
// is what others spent. Every daemon on the account sees the same drop, so
// all of them slow down together.
type Pacer struct {
	cfg PaceConfig

	mu        sync.Mutex
	have      bool
	prev      ghcli.RateLimitReading
	ownSince  float64
	otherRate float64
}

// NewPacer returns a pacer using cfg.
func NewPacer(cfg PaceConfig) *Pacer { return &Pacer{cfg: cfg} }

// Spend records points this daemon spent since the last observation.
func (p *Pacer) Spend(points float64) {
	p.mu.Lock()
	p.ownSince += points
	p.mu.Unlock()
}

// OtherRate returns the smoothed points per second other consumers spend.
func (p *Pacer) OtherRate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.otherRate
}

// Observe folds one reading into the other-consumer rate. A reading from a
// new window, or one that is not newer than the baseline, only resets or
// keeps the baseline.
func (p *Pacer) Observe(r ghcli.RateLimitReading) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.have || !p.prev.Reset.Equal(r.Reset) {
		p.have, p.prev, p.ownSince, p.otherRate = true, r, 0, 0
		return
	}
	dt := r.ObservedAt.Sub(p.prev.ObservedAt)
	if dt < p.cfg.MinObserveGap {
		return
	}
	others := float64(p.prev.Remaining-r.Remaining) - p.ownSince
	if others < 0 {
		others = 0
	}
	sample := others / dt.Seconds()
	p.otherRate = p.cfg.Smoothing*sample + (1-p.cfg.Smoothing)*p.otherRate
	p.prev, p.ownSince = r, 0
}

// Interval returns the poll interval for one poller. See the Pacer formula.
func (p *Pacer) Interval(now time.Time, r ghcli.RateLimitReading, base time.Duration, pollers int, cost float64) time.Duration {
	untilReset := r.Reset.Sub(now)
	if r.Limit <= 0 || untilReset <= 0 {
		return base
	}
	if pollers < 1 {
		pollers = 1
	}
	t := untilReset.Seconds()
	available := float64(r.Remaining) - p.cfg.Reserve*float64(r.Limit) - p.OtherRate()*t
	ceiling := untilReset
	if ceiling < base {
		ceiling = base
	}
	if available <= 0 {
		return ceiling
	}
	secs := float64(pollers) * cost * t / available
	if secs >= ceiling.Seconds() {
		return ceiling // also covers a quotient too large for a Duration
	}
	if d := time.Duration(secs * float64(time.Second)); d > base {
		return d
	}
	return base
}

// PollCandidate is one poller waiting for a turn.
type PollCandidate struct {
	ID          int
	LastRefresh time.Time
	Interval    time.Duration
}

// OldestDue returns the ID of the due candidate whose last refresh is the
// oldest, so no target starves while the budget is short. ok is false when
// no candidate is due.
func OldestDue(now time.Time, cands []PollCandidate) (id int, ok bool) {
	due := make([]PollCandidate, 0, len(cands))
	for _, c := range cands {
		if !now.Before(c.LastRefresh.Add(c.Interval)) {
			due = append(due, c)
		}
	}
	if len(due) == 0 {
		return 0, false
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].LastRefresh.Before(due[j].LastRefresh) })
	return due[0].ID, true
}
