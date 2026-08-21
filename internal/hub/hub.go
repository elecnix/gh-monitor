// Package hub multiplexes one fetch loop per monitored identity across many
// consumers. Each consumer maintains its own baseline snapshot so that
// consumption by one consumer never suppresses delivery to another — the core
// requirement behind gh-monitor issues #34 and #32.
//
// Today the hub supports PR identities. The shape is intentionally target-
// agnostic (a FetchFunc returns an already-distilled *monitor.PRStatus), so
// ref/issue/run/repo targets can be added by supplying a different FetchFunc
// without changing the multiplexer.
package hub

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// FetchFunc returns the current raw PR payload for one identity at the given
// query tier (see monitor.QueryTier). The hub shares this single fetch across
// every consumer; each consumer then distills it into a *monitor.PRStatus with
// its own SnapshotOptions (so per-consumer IgnoredBots are honored) before
// diffing against its own baseline. The tier lets the poller shed low-priority
// surfaces (annotations, reviews, comments) as the shared GraphQL budget runs
// low, keeping PR status + check outcomes alive.
type FetchFunc func(ctx context.Context, id resolver.Identity, tier monitor.QueryTier) (*monitor.PullRequest, error)

// RulesetFunc returns the required status checks for a repository. It is called
// once per poller (not per consumer) — rulesets rarely change mid-monitoring.
// When nil, the hub does not compute AwaitingChecks (the feature is disabled).
type RulesetFunc func(owner, repo string) (*monitor.RulesetChecks, error)

// Hub owns one poller goroutine per PR identity and fans each fetched snapshot
// out to every subscribed consumer. It is safe for concurrent use.
type Hub struct {
	fetch     FetchFunc
	rulesetFn RulesetFunc
	interval  time.Duration
	budget    *monitor.BudgetGuard

	mu      sync.Mutex
	pollers map[prKey]*prPoller

	// resumes holds watchers' baselines carried over from a predecessor
	// daemon (issue #73 handoff), keyed by the watcher's ResumeID, until the
	// watcher reconnects and claims it.
	resumes map[string]resumeEntry
	// brokerMu guards the optional broker-transport health flag and its
	// extended idle-poll ceiling. See SetBrokerHealth and SetBrokerIdleCap.
	brokerMu      sync.RWMutex
	brokerHealthy bool
	brokerIdleCap time.Duration // 0 = broker integration not enabled
}

// prKey identifies a single PR poller.
type prKey struct {
	owner  string
	repo   string
	number int
}

// New creates a Hub. interval is the cadence between background fetches; a
// poller also fetches immediately on start. If interval <= 0 it defaults to
// 60s. rulesetFn is called once when a poller starts to determine required
// status checks; pass nil to disable ruleset-aware monitoring. budget, when
// non-nil, makes every poller stretch its cadence as the shared GraphQL
// budget runs low (advisory; rate-limit errors keep their hard backoff).
func New(fetch FetchFunc, rulesetFn RulesetFunc, interval time.Duration, budget *monitor.BudgetGuard) *Hub {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Hub{
		fetch:     fetch,
		rulesetFn: rulesetFn,
		interval:  interval,
		budget:    budget,
		pollers:   make(map[prKey]*prPoller),
		resumes:   make(map[string]resumeEntry),
	}
}

// SubscribePR registers a consumer for the PR named by t and returns a channel
// of updates plus a cancel function. The consumer receives the poller's current
// snapshot immediately (as a first-poll against an empty baseline), then deltas
// on every subsequent fetch. Each consumer keeps its own baseline and its own
// snapshot filters, so one consumer never affects another.
//
// The hub emits backend.Update, not rendered notifications: it shares a fetch,
// it does not decide how anything reads. Rendering belongs to whoever owns the
// user's templates, which is the process the operator ran.
//
// cancel() detaches the consumer; the underlying poller stops once its last
// consumer leaves. cancel is idempotent and safe to call from multiple
// goroutines.
func (h *Hub) SubscribePR(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, func()) {
	identity := monitor.IdentityOf(t)
	key := prKey{identity.Owner, identity.Repo, identity.Number}

	h.mu.Lock()
	p := h.pollers[key]
	if p == nil {
		p = newPRPoller(h, identity, h.interval)
		h.pollers[key] = p
	}
	// A poller restored from a handoff already exists but has not started;
	// whether fresh or restored, it starts with its first subscriber.
	start := !p.started
	p.started = true
	h.mu.Unlock()

	if start {
		go p.run()
	}

	// The consumer only needs enough run configuration to diff and to stamp
	// the updates it emits; templates never reach the daemon.
	ro := monitor.RunOptions{Identity: identity}
	ro.Prefs.RetriggerComments = opts.RepeatUnresolved

	snapOpts := monitor.SnapshotOptions{IgnoredBots: opts.IgnoredAuthors}
	if len(opts.AnnotationLevels) > 0 {
		// Parsed, so "none" keeps meaning "report no annotations".
		if levels, err := monitor.ParseAnnotationLevels(strings.Join(opts.AnnotationLevels, ",")); err == nil {
			snapOpts.AnnotationLevels = levels
		}
	}

	sub := &prSub{
		consumer:   monitor.NewPRConsumer(ro),
		snapOpts:   snapOpts,
		snapshotCh: make(chan *monitor.PullRequest, 1),
		notifCh:    make(chan backend.Update, 4),
		out:        make(chan backend.Update, 16),
		done:       make(chan struct{}),
		resumeID:   opts.ResumeID,
		target:     t,
		watchOpts:  opts,
	}

	h.mu.Lock()
	// A watcher reconnecting after a daemon handoff claims the baseline its
	// predecessor's daemon held for it, so it resumes diffing where it left
	// off instead of replaying what it already reported (issue #73).
	if opts.ResumeID != "" {
		if baseline, ok := h.takeResume(opts.ResumeID); ok {
			sub.consumer.RestoreBaseline(baseline)
		}
	}
	h.mu.Unlock()

	// Use the poller's cached ruleset (fetched once on first run, or carried
	// over from a predecessor daemon), and start at the poller's current
	// tier. A poller restored from a handoff may already be at TierFull, in
	// which case applyTier never fires for this sub — and distilling its
	// snapshots at the zero tier would silently shed comments and reviews
	// exactly as if the budget had run out.
	p.mu.Lock()
	if p.ruleset != nil && p.ruleset.Error == "" {
		sub.snapOpts.RulesetChecks = p.ruleset
	}
	sub.snapOpts.Tier = p.tier
	p.subs[sub] = struct{}{}
	if p.latest != nil {
		select {
		case sub.snapshotCh <- p.latest:
		default:
		}
	}
	p.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() { h.detach(key, p, sub) })
	}
	// Also detach when the caller's context is cancelled.
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-sub.done:
		}
	}()
	go sub.loop()
	return sub.out, cancel
}

// RefreshPR triggers one immediate fetch+fanout for the poller backing the
// given identity, bypassing the interval timer. It is a no-op (returning
// nil) if no consumer is currently subscribed to that identity. The fetch
// runs on the poller goroutine, so RefreshPR returns as soon as the fetch is
// queued, not when it completes.
func (h *Hub) RefreshPR(id resolver.Identity) error {
	key := prKey{id.Owner, id.Repo, id.Number}
	h.mu.Lock()
	p := h.pollers[key]
	h.mu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// RefreshRepo wakes every currently-subscribed poller for owner/repo. It
// exists for broker events that name a repository but no specific PR —
// check_run and check_suite events are keyed by commit SHA, not PR number,
// so there is nothing to filter client-side by (the module README's
// pr_number guidance only applies when an event carries one). Refreshing
// every actively-watched PR in that repository is still far cheaper than
// the polling this transport replaces, and it stays authoritative: nothing
// is inferred from the fact the event fired, only that a fetch should run
// now instead of waiting for the next tick.
func (h *Hub) RefreshRepo(owner, repo string) {
	h.mu.Lock()
	var keys []prKey
	for k := range h.pollers {
		if k.owner == owner && k.repo == repo {
			keys = append(keys, k)
		}
	}
	h.mu.Unlock()
	for _, k := range keys {
		_ = h.RefreshPR(resolver.Identity{Owner: k.owner, Repo: k.repo, Number: k.number})
	}
}

// SetBrokerIdleCap enables broker-aware cadence stretching: while the
// broker transport is healthy (SetBrokerHealth), a poller's idle backoff is
// allowed to grow past monitor.MaxIdleInterval up to cap instead — polling
// becomes a rare safety net because a real change now wakes the poller
// immediately via RefreshPR/RefreshRepo. Call once at daemon startup,
// before any broker events arrive. cap <= 0 disables the override: pollers
// keep the normal ceiling even when the broker reports healthy.
func (h *Hub) SetBrokerIdleCap(cap time.Duration) {
	h.brokerMu.Lock()
	h.brokerIdleCap = cap
	h.brokerMu.Unlock()
}

// SetBrokerHealth records the broker transport's connection state and, on
// every transition, broadcasts a loud notice to every active subscriber
// across every poller — never silently. This is the "absence is not
// success" guarantee the transport exists to honour at the daemon level: a
// subscriber must be able to tell whether the low-latency wake path is live
// or whether interval polling alone is currently doing the work, because
// each state calls for a different response from whatever reads the
// notification stream. detail (typically an error's message) is appended
// to a degraded notice when non-empty; it is ignored for a healthy one.
//
// A transition to unhealthy takes effect on the very next poll: nextDelay
// stops honouring the extended idle cap immediately, so a lost broker
// connection is followed by normal polling within one cycle, not a stale
// long wait computed while the broker still looked healthy.
func (h *Hub) SetBrokerHealth(healthy bool, detail string) {
	h.brokerMu.Lock()
	changed := h.brokerHealthy != healthy
	h.brokerHealthy = healthy
	h.brokerMu.Unlock()
	if !changed {
		return
	}

	var msg string
	if healthy {
		msg = "✅ broker transport connected: PR/CI updates now arrive as they happen; polling continues as a rare safety net"
	} else {
		msg = "⚠️ broker transport degraded — falling back to interval polling until it reconnects"
		if detail != "" {
			msg += ": " + detail
		}
	}
	h.broadcastAll(monitor.Event{
		Type:   monitor.EventDegraded,
		Notice: msg,
	})
}

// BrokerHealthy reports the broker transport's last-known health and its
// configured extended idle cap (0 when SetBrokerIdleCap was never called,
// meaning the override never applies even if healthy is true).
func (h *Hub) BrokerHealthy() (healthy bool, idleCap time.Duration) {
	h.brokerMu.RLock()
	defer h.brokerMu.RUnlock()
	return h.brokerHealthy, h.brokerIdleCap
}

// broadcastAll fans an event out to every subscriber of every active
// poller — used for hub-wide notices (broker health) rather than a single
// poller's own degraded/tier-shed broadcasts.
func (h *Hub) broadcastAll(ev monitor.Event) {
	h.mu.Lock()
	pollers := make([]*prPoller, 0, len(h.pollers))
	for _, p := range h.pollers {
		pollers = append(pollers, p)
	}
	h.mu.Unlock()
	for _, p := range pollers {
		p.broadcast(ev)
	}
}

// Stop tears down every poller. It is safe to call multiple times.
func (h *Hub) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.pollers {
		p.stop()
	}
	h.pollers = map[prKey]*prPoller{}
}

// detach removes a subscriber from its poller and stops the poller when it has
// no consumers left.
func (h *Hub) detach(key prKey, p *prPoller, sub *prSub) {
	close(sub.done)
	var empty bool
	p.mu.Lock()
	delete(p.subs, sub)
	empty = len(p.subs) == 0
	p.mu.Unlock()
	if !empty {
		return
	}
	p.stop()
	h.mu.Lock()
	if cur := h.pollers[key]; cur == p {
		delete(h.pollers, key)
	}
	h.mu.Unlock()
}

// prPoller owns one fetch loop for one PR identity and broadcasts each fetched
// snapshot to its subscribers.
type prPoller struct {
	hub      *Hub
	identity resolver.Identity
	interval time.Duration
	budget   *monitor.BudgetGuard
	ruleset  *monitor.RulesetChecks // fetched once at poller start; nil until fetched

	// started records that run() is going. A poller restored from a handoff
	// (issue #73) exists before any subscriber does; it starts, like a fresh
	// one, when its first subscriber arrives. Guarded by hub.mu.
	started bool

	mu       sync.Mutex
	latest   *monitor.PullRequest
	noChange int               // consecutive fingerprint-unchanged fetches; drives idle backoff
	tier     monitor.QueryTier // last fetched tier; drives shed notices
	subs     map[*prSub]struct{}

	wake  chan struct{}
	stopc chan struct{}
	once  sync.Once
}

func newPRPoller(h *Hub, id resolver.Identity, interval time.Duration) *prPoller {
	return &prPoller{
		hub:      h,
		identity: id,
		interval: interval,
		budget:   h.budget,
		subs:     make(map[*prSub]struct{}),
		wake:     make(chan struct{}, 1),
		stopc:    make(chan struct{}),
	}
}

// run fetches ruleset once, then fetches immediately and on every tick or
// wake, until stop. The cadence backs off like the in-process loop's
// idleInterval: after three consecutive polls whose fingerprint is unchanged,
// the delay doubles up to the same 300s cap, so a quiet PR watched through
// the shared daemon costs the same GraphQL as one watched in-process. A wake
// (new subscriber, or an explicit RefreshPR) resets the backoff and fetches
// immediately.
func (p *prPoller) run() {
	// Fetch the branch ruleset once at startup — unless a predecessor daemon
	// handed one over (issue #73). Rulesets rarely change mid-monitoring; a
	// consumer that needs a refresh can restart.
	if p.hub.rulesetFn != nil && p.ruleset == nil {
		rs, err := p.hub.rulesetFn(p.identity.Owner, p.identity.Repo)
		if err != nil {
			// Log but continue — the poller still works without ruleset data.
			fmt.Fprintf(os.Stderr, "gh-monitor: ruleset fetch error: %v\n", err)
		}
		if rs != nil && rs.Error == "" {
			p.mu.Lock()
			p.ruleset = rs
			// Update all existing subscribers with the ruleset.
			for s := range p.subs {
				s.snapOpts.RulesetChecks = rs
			}
			p.mu.Unlock()
		}
	}
	p.fetchOnce()
	delay := p.nextDelay()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-p.stopc:
			return
		case <-timer.C:
			p.fetchOnce()
			delay = p.nextDelay()
			timer.Reset(delay)
		case <-p.wake:
			// A fresh subscriber wants the current state promptly; drop any
			// accumulated idle backoff so it gets one now, then a normal
			// cadence.
			p.mu.Lock()
			p.noChange = 0
			p.mu.Unlock()
			p.fetchOnce()
			delay = p.nextDelay()
			timer.Reset(delay)
		}
	}
}

// nextDelay returns the jittered idle-interval for the current noChange
// count, stretched by the advisory GraphQL budget when the guard reports the
// budget low. Jitter de-phases pollers that subscribed at the same moment, so
// a fleet attaching many watchers at once does not burst requests in phase.
//
// When the hub's optional broker transport reports healthy (and an idle cap
// was configured via SetBrokerIdleCap), the idle ceiling is extended well
// past monitor.MaxIdleInterval: a real change now arrives as an immediate
// wake instead of waiting for the next tick, so the periodic poll only has
// to serve as a rare safety net. The moment the broker degrades, this reads
// the normal ceiling again on the very next call — no stale extended wait
// survives a lost connection.
func (p *prPoller) nextDelay() time.Duration {
	p.mu.Lock()
	noChange := p.noChange
	p.mu.Unlock()

	ceiling := monitor.MaxIdleInterval
	if healthy, cap := p.hub.BrokerHealthy(); healthy && cap > 0 {
		ceiling = cap
	}

	d := monitor.Jittered(monitor.IdleIntervalCapped(p.interval, noChange, ceiling))
	if p.budget != nil {
		d += p.budget.Stretch(time.Now()).Extra
		if d > ceiling {
			d = ceiling
		}
	}
	return d
}

func (p *prPoller) stop() {
	p.once.Do(func() { close(p.stopc) })
}

// fetchOnce reads the current fetch function under the hub lock, fetches, and
// fans the snapshot out to every subscriber's snapshotCh. Sends are
// non-blocking: a consumer that has not drained its previous snapshot drops
// the intermediate one — acceptable for a polling monitor, which only ever
// observes states at poll boundaries anyway.
func (p *prPoller) fetchOnce() {
	p.hub.mu.Lock()
	fetch := p.hub.fetch
	p.hub.mu.Unlock()

	// Select the tier from the advisory GraphQL budget (TierFull when no
	// guard is wired) and say loudly when the watched surfaces change.
	tier := p.selectTier()
	if tier != p.tier {
		p.applyTier(tier)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Stop the fetch if the poller is stopped.
	go func() {
		select {
		case <-p.stopc:
			cancel()
		case <-ctx.Done():
		}
	}()

	curr, err := fetch(ctx, p.identity, tier)
	if err != nil {
		// A per-query resource-limit error can be beaten by a cheaper query:
		// retry once one tier down. A rate-limit 403 cannot (every query
		// costs points), so it falls through to the degraded broadcast.
		if monitor.IsQueryCostError(err) && tier > monitor.TierStatus {
			curr, err = fetch(ctx, p.identity, tier-1)
		}
		if err != nil || curr == nil {
			// Degrade loudly: a fetch error must reach subscribers, never
			// vanish — a silently blind watcher reads as "all clear". The
			// previous snapshot is retained; no inference replaces it.
			p.broadcast(monitor.Event{
				Type:            monitor.EventDegraded,
				DegradedSurface: "graphql",
				DegradedMessage: fmt.Sprintf("%v", err),
			})
			return
		}
	}
	// Track fingerprint changes so run() can idle-back off a quiet PR the way
	// the in-process loop does. Errors leave noChange untouched: a fetch that
	// failed tells us nothing about whether the PR changed.
	prevFp := ""
	p.mu.Lock()
	if p.latest != nil {
		prevFp = monitor.Fingerprint(p.latest)
	}
	if monitor.Fingerprint(curr) != prevFp {
		p.noChange = 0
	} else {
		p.noChange++
	}
	p.latest = curr
	for s := range p.subs {
		select {
		case s.snapshotCh <- curr:
		default:
		}
	}
	p.mu.Unlock()
}

// selectTier returns the fetch tier for the next poll: TierFull when no
// BudgetGuard is wired, otherwise derived from the advisory GraphQL budget.
func (p *prPoller) selectTier() monitor.QueryTier {
	if p.budget == nil {
		return monitor.TierFull
	}
	if remaining, limit, ok := p.budget.GraphQLRemaining(time.Now()); ok {
		return monitor.TierForRemaining(remaining, limit)
	}
	return monitor.TierFull
}

// applyTier records a tier change: it updates every subscriber's snapshot
// options (so ShedSurfaces flow into their snapshots) and broadcasts a loud
// notice naming what is no longer being watched (or that full monitoring has
// resumed). A watcher that quietly shed surfaces would turn a missing signal
// into an apparent all-clear.
func (p *prPoller) applyTier(tier monitor.QueryTier) {
	p.mu.Lock()
	p.tier = tier
	for s := range p.subs {
		s.snapOpts.Tier = tier
	}
	p.mu.Unlock()

	shed := tier.ShedSurfaces()
	var msg string
	if len(shed) > 0 {
		msg = fmt.Sprintf("⚠️ GraphQL budget low on %s: no longer watching %s until the budget recovers",
			p.label(), strings.Join(shed, ", "))
	} else {
		msg = fmt.Sprintf("✅ GraphQL budget recovered on %s: resuming full monitoring", p.label())
	}
	p.broadcast(monitor.Event{Type: monitor.EventDegraded, Notice: msg})
}

// broadcast fans a loop-level event out to every subscriber. Sends are
// non-blocking: a consumer that has not drained its previous notice drops the
// intermediate one, which is acceptable for state-change notices (the next
// tier change or error re-notifies).
func (p *prPoller) broadcast(ev monitor.Event) {
	u := backend.Update{
		Target: monitor.TargetOf(p.identity),
		Event:  ev,
		At:     time.Now(),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for s := range p.subs {
		select {
		case s.notifCh <- u:
		default:
		}
	}
}

// label renders the identity as "owner/repo#number" for notifications.
func (p *prPoller) label() string {
	return fmt.Sprintf("%s/%s#%d", p.identity.Owner, p.identity.Repo, p.identity.Number)
}

// prSub is one consumer's handle: its own PRConsumer (baseline), a buffered
// snapshot inbox, a channel for loop-level notices (degraded / tier-shed), and
// an output channel of updates.
//
// mu guards consumer, the one field mutated after creation (by loop's Consume
// and by ExportState's baseline read on the handoff path).
type prSub struct {
	mu         sync.Mutex
	consumer   *monitor.PRConsumer
	snapOpts   monitor.SnapshotOptions
	snapshotCh chan *monitor.PullRequest
	notifCh    chan backend.Update
	out        chan backend.Update
	done       chan struct{}
	resumeID   string
	target     backend.Target
	watchOpts  backend.WatchOptions
}

// loop owns the consumer's diff/render goroutine. It reads raw PR payloads
// from the poller, distills each into a PRStatus with the consumer's own
// SnapshotOptions (so IgnoredBots stay per-consumer), diffs against the
// consumer's own baseline, and emits notifications until done is closed or
// the PR reaches a terminal state (merged/closed), at which point it closes
// the output channel so daemon clients get a clean EOF.
func (s *prSub) loop() {
	for {
		select {
		case raw, ok := <-s.snapshotCh:
			if !ok {
				close(s.out)
				return
			}
			curr := monitor.Snapshot(raw, s.snapOpts)
			s.mu.Lock()
			terminal := s.consumer.Consume(curr, func(u backend.Update) {
				select {
				case s.out <- u:
				case <-s.done:
				}
			})
			s.mu.Unlock()
			if terminal {
				close(s.out)
				return
			}
		case u := <-s.notifCh:
			// Loop-level notices (degraded, tier-shed) pass straight through.
			select {
			case s.out <- u:
			case <-s.done:
			}
		case <-s.done:
			close(s.out)
			return
		}
	}
}
