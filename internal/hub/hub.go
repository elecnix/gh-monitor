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
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// FetchFunc returns the current raw PR payload for one identity. The hub
// shares this single fetch across every consumer; each consumer then distills
// it into a *monitor.PRStatus with its own SnapshotOptions (so per-consumer
// IgnoredBots are honored) before diffing against its own baseline.
type FetchFunc func(ctx context.Context, id resolver.Identity) (*monitor.PullRequest, error)

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
	}
}

// SubscribePR registers a consumer for the PR identified by opts.Identity and
// returns a channel of notifications plus a cancel function. The consumer
// receives the poller's current snapshot immediately (as a first-poll against
// an empty baseline), then deltas on every subsequent fetch. Each consumer
// keeps its own baseline, so one consumer advancing never affects another.
//
// cancel() detaches the consumer; the underlying poller stops once its last
// consumer leaves. cancel is idempotent and safe to call from multiple
// goroutines.
func (h *Hub) SubscribePR(ctx context.Context, opts monitor.RunOptions) (<-chan monitor.Notification, func()) {
	key := prKey{opts.Identity.Owner, opts.Identity.Repo, opts.Identity.Number}

	h.mu.Lock()
	p := h.pollers[key]
	if p == nil {
		p = newPRPoller(h, opts.Identity, h.interval)
		h.pollers[key] = p
		go p.run()
	}
	h.mu.Unlock()

	sub := &prSub{
		consumer:   monitor.NewPRConsumer(opts),
		snapOpts:   monitor.SnapshotOptions{IgnoredBots: opts.Prefs.IgnoredBots, AnnotationLevels: opts.AnnotationLevels},
		snapshotCh: make(chan *monitor.PullRequest, 1),
		out:        make(chan monitor.Notification, 16),
		done:       make(chan struct{}),
	}
	go sub.loop()

	// Use the poller's cached ruleset (fetched once on first run).
	p.mu.Lock()
	if p.ruleset != nil && p.ruleset.Error == "" {
		sub.snapOpts.RulesetChecks = p.ruleset
	}
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

	mu       sync.Mutex
	latest   *monitor.PullRequest
	noChange int // consecutive fingerprint-unchanged fetches; drives idle backoff
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
	// Fetch the branch ruleset once at startup. Rulesets rarely change
	// mid-monitoring; a consumer that needs a refresh can restart.
	if p.hub.rulesetFn != nil {
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
func (p *prPoller) nextDelay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := monitor.Jittered(monitor.IdleInterval(p.interval, p.noChange))
	if p.budget != nil {
		d += p.budget.Stretch(time.Now()).Extra
		if d > monitor.MaxIdleInterval {
			d = monitor.MaxIdleInterval
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

	curr, err := fetch(ctx, p.identity)
	if err != nil || curr == nil {
		return
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

// prSub is one consumer's handle: its own PRConsumer (baseline), a buffered
// snapshot inbox, and an output channel of notifications.
type prSub struct {
	consumer   *monitor.PRConsumer
	snapOpts   monitor.SnapshotOptions
	snapshotCh chan *monitor.PullRequest
	out        chan monitor.Notification
	done       chan struct{}
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
			terminal := s.consumer.Consume(curr, func(n monitor.Notification) {
				select {
				case s.out <- n:
				case <-s.done:
				}
			})
			if terminal {
				close(s.out)
				return
			}
		case <-s.done:
			close(s.out)
			return
		}
	}
}