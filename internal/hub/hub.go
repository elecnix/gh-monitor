// Package hub multiplexes one fetch loop per monitored identity across many
// consumers. Each consumer maintains its own baseline snapshot so that
// consumption by one consumer never suppresses delivery to another — the core
// requirement behind gh-monitor issues #34 and #32.
//
// The hub is target-kind agnostic: one poller serves any identity (pull
// request, ref, commit, issue, workflow run, or whole repository), and a
// per-kind trait table says how to distill that kind's raw payload into a
// status, how to fingerprint it for idle backoff, and which fetch tiers it
// honours. The per-poll diff behaviour lives in the monitor package's
// consumers the in-process loops used to share before they were deleted —
// one event vocabulary, one code path (issue #76).
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// FetchFunc returns the current raw payload for one identity at the given
// query tier (see monitor.QueryTier). The hub shares this single fetch across
// every consumer; each consumer then distills it into its own status with its
// own snapshot options (so per-consumer IgnoredBots are honored) before
// diffing against its own baseline. The tier lets the poller shed low-priority
// surfaces (annotations, reviews, comments) as the shared GraphQL budget runs
// low, keeping status + check outcomes alive.
//
// The concrete payload type depends on the identity's target kind, matching
// what the monitor.Service fetch for that kind returns:
//
//	pr     *monitor.PullRequest
//	ref    *monitor.RefQueryResponse
//	commit *monitor.CommitQueryResponse
//	issue  *monitor.IssueQueryResponse
//	run    *monitor.WorkflowRun
//	repo   *monitor.RepoQueryResponse
type FetchFunc func(ctx context.Context, id resolver.Identity, tier monitor.QueryTier) (any, error)

// RulesetFunc returns the required status checks for a repository. It is called
// once per PR poller (not per consumer) — rulesets rarely change
// mid-monitoring. When nil, the hub does not compute AwaitingChecks (the
// feature is disabled). It is never called for non-PR kinds: no other kind
// consumes ruleset data.
type RulesetFunc func(owner, repo string) (*monitor.RulesetChecks, error)

// FailedRunLogFetcher fetches the failed-run log snippet for one workflow run.
// It is injected because the fetch goes through the gh CLI, whose client lives
// with the caller (the daemon); without it, failed-run notifications carry no
// log snippet.
type FailedRunLogFetcher func(owner, repo string, runID int) (string, error)

// Option configures optional Hub behaviour.
type Option func(*Hub)

// WithFailedRunLogFetcher installs the daemon-side failed-run log fetcher used
// by run-target subscribers. See FailedRunLogFetcher.
func WithFailedRunLogFetcher(fn FailedRunLogFetcher) Option {
	return func(h *Hub) { h.failedLogs = fn }
}

// WithIdleCeiling overrides the idle-backoff ceiling every poller uses —
// the idlePollCeiling preference (issue #90). It replaces
// monitor.MaxIdleInterval for all targets, broker-healthy or not; a value
// <= 0 keeps the built-in default.
func WithIdleCeiling(ceiling time.Duration) Option {
	return func(h *Hub) { h.idleCeiling = ceiling }
}

// WithPauseWhenBrokerHealthy arms pollWhenBrokerHealthy: false (issue #90).
// While set and the broker transport reports healthy, timer-driven fetching
// suspends entirely — scheduled API spend exists only as insurance against
// event loss. A degrade resumes polling immediately (SetBrokerHealth wakes
// every poller on the transition), and while paused the timer still ticks at
// roughly the base interval so a missed transition is noticed within one tick.
func WithPauseWhenBrokerHealthy(pause bool) Option {
	return func(h *Hub) { h.pauseWhenBrokerHealthy = pause }
}

// Hub owns one poller goroutine per identity and fans each fetched snapshot
// out to every subscribed consumer. It is safe for concurrent use.
type Hub struct {
	fetch      FetchFunc
	rulesetFn  RulesetFunc
	failedLogs FailedRunLogFetcher
	interval   time.Duration
	budget     *monitor.BudgetGuard
	// idleCeiling is the configured idle-backoff ceiling (idlePollCeiling,
	// issue #90); 0 means monitor.MaxIdleInterval.
	idleCeiling            time.Duration
	pauseWhenBrokerHealthy bool

	mu      sync.Mutex
	pollers map[pollerKey]*poller

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

// pollerKey identifies a single poller. Every field a resolver.Identity can
// distinguish on is part of the key: a ref named "main" and a PR numbered 7
// in the same repo are different pollers even though they share owner/repo.
type pollerKey struct {
	kind   backend.Kind
	owner  string
	repo   string
	number int
	ref    string
	sha    string
	runID  int
}

// keyOf derives a poller key from a resolved identity.
func keyOf(id resolver.Identity) pollerKey {
	kind := backend.Kind(id.Target)
	if kind == "" {
		kind = backend.KindPR
	}
	return pollerKey{
		kind:   kind,
		owner:  id.Owner,
		repo:   id.Repo,
		number: id.Number,
		ref:    id.Ref,
		sha:    id.CommitSHA,
		runID:  id.RunID,
	}
}

// New creates a Hub. interval is the cadence between background fetches; a
// poller also fetches immediately on start. If interval <= 0 it defaults to
// 60s. rulesetFn is called once when a PR poller starts to determine required
// status checks; pass nil to disable ruleset-aware monitoring. budget, when
// non-nil, makes every poller stretch its cadence as the shared GraphQL
// budget runs low (advisory; rate-limit errors keep their hard backoff).
// Options configure the rest (see Option).
func New(fetch FetchFunc, rulesetFn RulesetFunc, interval time.Duration, budget *monitor.BudgetGuard, opts ...Option) *Hub {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	h := &Hub{
		fetch:     fetch,
		rulesetFn: rulesetFn,
		interval:  interval,
		budget:    budget,
		pollers:   make(map[pollerKey]*poller),
		resumes:   make(map[string]resumeEntry),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Subscribe registers a consumer for the target named by t and returns a
// channel of updates plus a cancel function. The consumer receives the
// poller's current snapshot immediately (as a first-poll against an empty
// baseline — or against opts.Baseline when resuming a named instance), then
// deltas on every subsequent fetch. Each consumer keeps its own baseline and
// its own snapshot filters, so one consumer never affects another.
//
// The hub emits backend.Update, not rendered notifications: it shares a fetch,
// it does not decide how anything reads. Rendering belongs to whoever owns the
// user's templates, which is the process the operator ran.
//
// cancel() detaches the consumer; the underlying poller stops once its last
// consumer leaves. cancel is idempotent and safe to call from multiple
// goroutines.
func (h *Hub) Subscribe(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, func()) {
	identity := monitor.IdentityOf(t)
	key := keyOf(identity)
	traits := traitsFor(key.kind)

	h.mu.Lock()
	p := h.pollers[key]
	if p == nil {
		p = newPoller(h, identity, h.interval)
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

	sub := &sub{
		distill:    traits.distill(t, opts),
		handle:     traits.consumer(h, ro, opts),
		cursorOf:   traits.cursorOf,
		snapOpts:   snapOpts,
		snapshotCh: make(chan any, 1),
		notifCh:    make(chan backend.Update, 4),
		out:        make(chan backend.Update, 16),
		done:       make(chan struct{}),
		resumeID:   opts.ResumeID,
		target:     t,
		watchOpts:  opts,
	}

	// A watcher reconnecting after a daemon handoff claims the baseline its
	// predecessor's daemon held for it, so it resumes diffing where it left
	// off instead of replaying what it already reported (issue #73).
	if opts.ResumeID != "" {
		if raw, ok := h.takeResume(opts.ResumeID); ok && len(raw) > 0 {
			st := newStatusForKind(key.kind)
			if err := json.Unmarshal(raw, st); err == nil {
				sub.handle.restore(st)
			}
		}
	}
	go sub.loop()

	// Use the poller's cached ruleset (fetched once on first run).
	p.mu.Lock()
	if p.ruleset != nil && p.ruleset.Error == "" {
		sub.snapOpts.RulesetChecks = p.ruleset
	}
	// Start at the poller's current tier. A poller restored from a handoff
	// may already be at TierFull, in which case applyTier never fires for
	// this sub — and distilling its snapshots at the zero tier would silently
	// shed comments and reviews exactly as if the budget had run out.
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
	return sub.out, cancel
}

// SubscribePR registers a consumer for the pull request named by t. It is a
// thin wrapper over Subscribe, kept because it reads better at call sites
// that only ever watch pull requests.
func (h *Hub) SubscribePR(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, func()) {
	return h.Subscribe(ctx, t, opts)
}

// Refresh triggers one immediate fetch+fanout for the poller backing the
// given identity, bypassing the interval timer. It is a no-op (returning
// nil) if no consumer is currently subscribed to that identity. The fetch
// runs on the poller goroutine, so Refresh returns as soon as the fetch is
// queued, not when it completes.
func (h *Hub) Refresh(id resolver.Identity) error {
	key := keyOf(id)
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

// RefreshPR is Refresh restricted (by name) to pull-request identities.
func (h *Hub) RefreshPR(id resolver.Identity) error { return h.Refresh(id) }

// RefreshRepo wakes every currently-subscribed poller for owner/repo, whatever
// kind each watches. It exists for broker events that name a repository but no
// specific target — check_run and check_suite events are keyed by commit SHA,
// not PR number, so there is nothing to filter client-side by (the module
// README's pr_number guidance only applies when an event carries one).
// Refreshing every actively-watched target in that repository is still far
// cheaper than the polling this transport replaces, and it stays
// authoritative: nothing is inferred from the fact the event fired, only that
// a fetch should run now instead of waiting for the next tick.
func (h *Hub) RefreshRepo(owner, repo string) {
	h.mu.Lock()
	var keys []pollerKey
	for k := range h.pollers {
		if k.owner == owner && k.repo == repo {
			keys = append(keys, k)
		}
	}
	h.mu.Unlock()
	for _, k := range keys {
		_ = h.Refresh(resolver.Identity{
			Owner: k.owner, Repo: k.repo, Target: string(k.kind),
			Number: k.number, Ref: k.ref, CommitSHA: k.sha, RunID: k.runID,
		})
	}
}

// Once performs a single fetch of the target and emits its current actionable
// state — the hub's equivalent of the deleted in-process WatchOnce paths. It fetches
// once at the full tier, distills with the caller's snapshot options, diffs
// against opts.Baseline (empty means an empty baseline, which is a
// first-poll), and closes the returned channel when done. No poller is
// started and nothing is cached: a Once never affects a concurrent Subscribe,
// and vice versa.
func (h *Hub) Once(ctx context.Context, t backend.Target, opts backend.WatchOptions) <-chan backend.Update {
	identity := monitor.IdentityOf(t)
	traits := traitsFor(keyOf(identity).kind)

	ro := monitor.RunOptions{Identity: identity}
	ro.Prefs.RetriggerComments = opts.RepeatUnresolved

	snapOpts := monitor.SnapshotOptions{IgnoredBots: opts.IgnoredAuthors}
	if len(opts.AnnotationLevels) > 0 {
		if levels, err := monitor.ParseAnnotationLevels(strings.Join(opts.AnnotationLevels, ",")); err == nil {
			snapOpts.AnnotationLevels = levels
		}
	}
	if h.rulesetFn != nil && keyOf(identity).kind == backend.KindPR {
		if rs, err := h.rulesetFn(identity.Owner, identity.Repo); err == nil && rs != nil && rs.Error == "" {
			snapOpts.RulesetChecks = rs
		}
	}

	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		h.mu.Lock()
		fetch := h.fetch
		h.mu.Unlock()

		raw, err := fetch(ctx, identity, monitor.TierFull)
		if err != nil {
			// A one-shot read has no next poll to recover on, so a fetch
			// error is the answer: report it as degraded and stop.
			out <- backend.Update{
				Target: monitor.TargetOf(identity),
				Event: backend.Event{
					Type:            backend.EventDegraded,
					DegradedSurface: "graphql",
					DegradedMessage: err.Error(),
				},
				At: time.Now(),
			}
			return
		}

		distill := traits.distill(t, opts)
		handle := traits.consumer(h, ro, opts)
		cursorOf := traits.cursorOf
		emit := func(u backend.Update) {
			if cursorOf != nil {
				u.Cursor = cursorOf(raw)
			}
			select {
			case out <- u:
			case <-ctx.Done():
			}
		}
		handle.consume(distill(raw, snapOpts), emit)
	}()
	return out
}

// SetBrokerIdleCap enables broker-aware cadence stretching: while the
// broker transport is healthy (SetBrokerHealth), a poller's idle backoff is
// allowed to grow past monitor.MaxIdleInterval up to cap instead — polling
// becomes a rare safety net because a real change now wakes the poller
// immediately via Refresh/RefreshRepo. Call once at daemon startup,
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
//	A transition to unhealthy takes effect on the very next poll: nextDelay
//
// stops honouring the extended idle cap immediately, so a lost broker
// connection is followed by normal polling within one cycle, not a stale
// extended wait computed while the broker still looked healthy. The
// transition itself also wakes every poller, so the first post-degrade fetch
// happens now rather than at whatever tick was scheduled while the wake path
// still looked live — with pollWhenBrokerHealthy: false (issue #90) that
// tick can be arbitrarily far away.
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
	if !healthy {
		// Insurance polling resumes now: wake every poller for an immediate
		// fetch instead of waiting out a pause-era tick. Non-blocking sends;
		// a poller mid-fetch picks the wake up on its next loop iteration.
		h.wakeAll()
	}
}

// wakeAll queues a refresh wake on every active poller. Used on the broker's
// healthy→degraded transition so the safety-net path re-engages immediately.
func (h *Hub) wakeAll() {
	h.mu.Lock()
	pollers := make([]*poller, 0, len(h.pollers))
	for _, p := range h.pollers {
		pollers = append(pollers, p)
	}
	h.mu.Unlock()
	for _, p := range pollers {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
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
	pollers := make([]*poller, 0, len(h.pollers))
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
	h.pollers = map[pollerKey]*poller{}
}

// detach removes a subscriber from its poller and stops the poller when it has
// no consumers left.
func (h *Hub) detach(key pollerKey, p *poller, s *sub) {
	close(s.done)
	var empty bool
	p.mu.Lock()
	delete(p.subs, s)
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

// ---------------------------------------------------------------------------
// Per-kind traits
// ---------------------------------------------------------------------------

// kindTraits tells the hub everything kind-specific about a target: how to
// distill a raw payload into a per-consumer status, how to build that
// consumer's diff engine, how to fingerprint a raw payload for idle backoff,
// and which fetch tiers the kind's queries honour. Everything else — poller
// lifecycle, fan-out, backoff, degraded broadcasts — is shared.
type kindTraits struct {
	// distill returns the per-subscriber distillation. It may close over the
	// target and watch options (repo targets clip by the subscriber's cursor
	// position; PR targets carry annotation levels and ruleset checks).
	distill func(t backend.Target, opts backend.WatchOptions) func(raw any, snapOpts monitor.SnapshotOptions) backend.Status

	// consumer builds the subscriber's diff engine — one of the monitor
	// package's per-kind consumers — wrapped in a kind-agnostic handle. The
	// handle's consume closure returns true when the target reached a
	// terminal state; its baseline accessors exist for the upgrade handoff
	// (issue #73).
	consumer func(h *Hub, ro monitor.RunOptions, opts backend.WatchOptions) consumerHandle

	// fingerprint covers every field the kind's diff can fire on. It runs on
	// the poller's raw payload once per fetch.
	fingerprint func(raw any) string

	// tiered reports whether the kind's fetch honours query tiers.
	tiered bool

	// cursorOf, when non-nil, extracts the opaque resume token an update
	// should carry so the client can persist its cursor (repo targets:
	// the latest createdAt in the response).
	cursorOf func(raw any) string
}

// consumerHandle is one subscriber's diff engine: the consume closure over a
// typed monitor consumer, plus the baseline access the upgrade handoff needs
// (issue #73) — baseline reads the last state shown to this watcher, restore
// seeds it so a reconnecting watcher resumes where its predecessor's stream
// left off instead of replaying what it already reported.
type consumerHandle struct {
	consume  func(curr backend.Status, emit func(backend.Update)) bool
	baseline func() backend.Status
	restore  func(backend.Status)
}

// newHandle wraps a constructed typed consumer into a consumerHandle,
// optionally seeding its baseline from stored JSON (the WatchOptions.Baseline
// resume path, issue #32).
func newHandle[S any, P interface {
	*S
	backend.Status
}](c interface {
	Consume(P, func(backend.Update)) bool
	RestoreBaseline(P)
	Snapshot() P
}, baseline string) consumerHandle {
	if baseline != "" {
		var stored S
		if err := json.Unmarshal([]byte(baseline), &stored); err == nil {
			c.RestoreBaseline(&stored)
		}
	}
	return consumerHandle{
		consume:  consumeVia(c),
		baseline: func() backend.Status { return c.Snapshot() },
		restore:  func(st backend.Status) { c.RestoreBaseline(st.(P)) },
	}
}

// newStatusForKind returns the zero-valued distilled status for a kind, so a
// handoff-carried baseline (JSON) can be decoded without knowing the kind at
// the call site.
func newStatusForKind(kind backend.Kind) backend.Status {
	switch kind {
	case backend.KindRef, backend.KindCommit:
		return &monitor.RefStatus{}
	case backend.KindIssue:
		return &monitor.IssueStatus{}
	case backend.KindRun:
		return &monitor.RunStatus{}
	case backend.KindRepo:
		return &monitor.RepoStatus{}
	default:
		return &monitor.PRStatus{}
	}
}

// rawForKind returns a zero-valued raw payload of the kind a fetch for this
// identity returns, so a handoff-carried snapshot (JSON) can be decoded into
// the concrete type the poller's traits expect.
func rawForKind(kind backend.Kind) any {
	switch kind {
	case backend.KindRef:
		return &monitor.RefQueryResponse{}
	case backend.KindCommit:
		return &monitor.CommitQueryResponse{}
	case backend.KindIssue:
		return &monitor.IssueQueryResponse{}
	case backend.KindRun:
		return &monitor.WorkflowRun{}
	case backend.KindRepo:
		return &monitor.RepoQueryResponse{}
	default:
		return &monitor.PullRequest{}
	}
}

// traitsFor returns the trait table for a target kind. The per-kind pieces
// are named package functions (below) rather than inline closures so each
// appears exactly once — the duplicate-code detector reads this table, and
// so should you.
func traitsFor(kind backend.Kind) kindTraits {
	switch kind {
	case backend.KindRef:
		return kindTraits{
			distill:     plainDistill(distillRef),
			consumer:    refCommitConsumer,
			fingerprint: fingerprintRefResp,
			tiered:      true,
		}
	case backend.KindCommit:
		return kindTraits{
			distill:     plainDistill(distillCommit),
			consumer:    refCommitConsumer,
			fingerprint: fingerprintCommitResp,
			tiered:      true,
		}
	case backend.KindIssue:
		return kindTraits{
			distill: snapDistill(distillIssue),
			consumer: func(_ *Hub, ro monitor.RunOptions, opts backend.WatchOptions) consumerHandle {
				return newHandle(monitor.NewIssueConsumer(ro), opts.Baseline)
			},
			fingerprint: fingerprintIssueResp,
		}
	case backend.KindRun:
		return kindTraits{
			distill: plainDistill(distillRun),
			consumer: func(h *Hub, ro monitor.RunOptions, _ backend.WatchOptions) consumerHandle {
				c := monitor.NewRunConsumer(ro)
				// The failed-run log snippet is fetched through the gh CLI,
				// which lives on the daemon side of the wire; the consumer
				// only needs the distilled detail.
				c.FailedLogDetail = h.failedRunLogDetail(ro.Identity)
				return newHandle(c, "")
			},
			fingerprint: fingerprintRunResp,
		}
	case backend.KindRepo:
		return kindTraits{
			distill:     repoDistill,
			consumer:    repoConsumerHandle,
			fingerprint: fingerprintRepoResp,
			cursorOf:    repoCursor,
		}
	default: // backend.KindPR
		return kindTraits{
			distill: snapDistill(distillPR),
			consumer: func(_ *Hub, ro monitor.RunOptions, opts backend.WatchOptions) consumerHandle {
				return newHandle(monitor.NewPRConsumer(ro), opts.Baseline)
			},
			fingerprint: func(raw any) string {
				return monitor.Fingerprint(raw.(*monitor.PullRequest))
			},
			tiered: true,
		}
	}
}

// plainDistill adapts a distillation that ignores the snapshot options (all
// kinds except PR and issue) to the trait signature.
func plainDistill(fn func(raw any) backend.Status) func(backend.Target, backend.WatchOptions) func(any, monitor.SnapshotOptions) backend.Status {
	return snapDistill(func(raw any, _ monitor.SnapshotOptions) backend.Status {
		return fn(raw)
	})
}

// snapDistill adapts a distillation that reads the subscriber's snapshot
// options (PR: annotation levels; issue: ignored bots) to the trait
// signature.
func snapDistill(fn func(raw any, snapOpts monitor.SnapshotOptions) backend.Status) func(backend.Target, backend.WatchOptions) func(any, monitor.SnapshotOptions) backend.Status {
	return func(_ backend.Target, _ backend.WatchOptions) func(any, monitor.SnapshotOptions) backend.Status {
		return fn
	}
}

func distillRef(raw any) backend.Status {
	return monitor.SnapshotRef(raw.(*monitor.RefQueryResponse).Repository.Ref)
}

func distillCommit(raw any) backend.Status {
	return monitor.SnapshotCommit(raw.(*monitor.CommitQueryResponse).Repository.Object)
}

func distillIssue(raw any, snapOpts monitor.SnapshotOptions) backend.Status {
	return monitor.SnapshotIssue(raw.(*monitor.IssueQueryResponse).Repository.Issue,
		monitor.SnapshotOptions{IgnoredBots: snapOpts.IgnoredBots})
}

func distillRun(raw any) backend.Status {
	return monitor.SnapshotRun(raw.(*monitor.WorkflowRun))
}

func distillPR(raw any, snapOpts monitor.SnapshotOptions) backend.Status {
	return monitor.Snapshot(raw.(*monitor.PullRequest), snapOpts)
}

// repoDistill distills a repo response, clipping it to the subscriber's
// cursor position first. An empty Since means "no cursor": everything is
// surfaced. A named instance with no cursor passes "now" as its Since, so
// it starts from where it was created.
func repoDistill(t backend.Target, opts backend.WatchOptions) func(any, monitor.SnapshotOptions) backend.Status {
	since := opts.Since
	return func(raw any, _ monitor.SnapshotOptions) backend.Status {
		resp := raw.(*monitor.RepoQueryResponse)
		if since != "" {
			resp = monitor.ClipRepoResponse(resp, since)
		}
		return monitor.SnapshotRepo(resp)
	}
}

// refCommitConsumer builds the diff engine shared by ref and commit targets:
// both distill to a RefStatus and diff with the same rules.
func refCommitConsumer(_ *Hub, ro monitor.RunOptions, _ backend.WatchOptions) consumerHandle {
	return newHandle(monitor.NewRefConsumer(ro), "")
}

// repoConsumerHandle builds the diff engine for a repository watch.
func repoConsumerHandle(_ *Hub, ro monitor.RunOptions, _ backend.WatchOptions) consumerHandle {
	return newHandle(monitor.NewRepoConsumer(ro), "")
}

// consumeVia adapts one of the monitor package's typed consumers to the
// kind-agnostic consume closure the trait table stores. P exists only to
// tell the compiler that *S really is a backend.Status.
func consumeVia[S any, P interface {
	*S
	backend.Status
}](c interface {
	Consume(P, func(backend.Update)) bool
}) func(backend.Status, func(backend.Update)) bool {
	return func(curr backend.Status, emit func(backend.Update)) bool {
		return c.Consume(curr.(P), emit)
	}
}

func fingerprintRefResp(raw any) string {
	return monitor.FingerprintRef(monitor.SnapshotRef(raw.(*monitor.RefQueryResponse).Repository.Ref))
}

func fingerprintCommitResp(raw any) string {
	return monitor.FingerprintRef(monitor.SnapshotCommit(raw.(*monitor.CommitQueryResponse).Repository.Object))
}

func fingerprintIssueResp(raw any) string {
	return monitor.FingerprintIssue(monitor.SnapshotIssue(raw.(*monitor.IssueQueryResponse).Repository.Issue,
		monitor.SnapshotOptions{}))
}

func fingerprintRunResp(raw any) string {
	return monitor.FingerprintRun(monitor.SnapshotRun(raw.(*monitor.WorkflowRun)))
}

func fingerprintRepoResp(raw any) string {
	return monitor.FingerprintRepo(monitor.SnapshotRepo(raw.(*monitor.RepoQueryResponse)))
}

// repoCursor extracts the opaque resume token a repo update carries: the
// latest createdAt in the response, so the client can advance its cursor
// exactly as runRepo's AdvanceCursor did.
func repoCursor(raw any) string {
	return monitor.LatestRepoCreatedAt(raw.(*monitor.RepoQueryResponse))
}

// failedRunLogDetail builds the failed-run log snippet fetcher a run consumer
// needs, bound to the hub's injected FailedRunLogFetcher (WithFailedRunLogFetcher).
// Without one, failed-run notifications carry no log snippet — degraded, not
// broken.
func (h *Hub) failedRunLogDetail(id resolver.Identity) func(runID int) string {
	return func(runID int) string {
		if h.failedLogs == nil {
			return ""
		}
		out, err := h.failedLogs(id.Owner, id.Repo, runID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gh-monitor: failed-run log fetch error: %v\n", err)
			return ""
		}
		return monitor.SummarizeFailedLog(out, monitor.MaxFailedLogLines)
	}
}

// ---------------------------------------------------------------------------
// Poller
// ---------------------------------------------------------------------------

// poller owns one fetch loop for one identity and broadcasts each fetched
// snapshot to its subscribers.
type poller struct {
	hub      *Hub
	identity resolver.Identity
	key      pollerKey
	traits   kindTraits
	interval time.Duration
	budget   *monitor.BudgetGuard
	ruleset  *monitor.RulesetChecks // fetched once at PR poller start; nil until fetched

	// started records that run() is going. A poller restored from a handoff
	// (issue #73) exists before any subscriber does; it starts, like a fresh
	// one, when its first subscriber arrives. Guarded by hub.mu.
	started bool

	mu         sync.Mutex
	latest     any               // raw payload of the last successful fetch
	noChange   int               // consecutive fingerprint-unchanged fetches; drives idle backoff
	tier       monitor.QueryTier // last fetched tier; drives shed notices
	degraded   map[string]string // surface -> last emitted error message; drives degraded-episode dedup (issue #66)
	errBackoff time.Duration     // consecutive-failure backoff; doubles per failed fetch, resets on success
	subs       map[*sub]struct{}

	wake  chan struct{}
	stopc chan struct{}
	once  sync.Once
}

func newPoller(h *Hub, id resolver.Identity, interval time.Duration) *poller {
	key := keyOf(id)
	return &poller{
		hub:      h,
		identity: id,
		key:      key,
		traits:   traitsFor(key.kind),
		interval: interval,
		budget:   h.budget,
		subs:     make(map[*sub]struct{}),
		wake:     make(chan struct{}, 1),
		stopc:    make(chan struct{}),
	}
}

// run fetches ruleset (PR only) once, then fetches immediately and on every
// tick or wake, until stop. The cadence backs off like the in-process loop's
// idleInterval: after three consecutive polls whose fingerprint is unchanged,
// the delay doubles up to the idle ceiling (idlePollCeiling when configured,
// otherwise 300s — issue #90), so a quiet target watched through the shared
// daemon costs the same GraphQL as one watched in-process. A wake
// (new subscriber, an explicit Refresh, or a broker degrade while pausing is
// armed) resets the backoff and fetches immediately.
//
// With pollWhenBrokerHealthy: false and the broker reporting healthy, timer
// ticks carry no fetch at all — the loop just re-arms near the base interval
// until the pause lifts. Wakes always fetch: event-driven updates are the
// path this policy trusts.
func (p *poller) run() {
	// Fetch the branch ruleset once at startup (PR targets only — no other
	// kind consumes ruleset data) — unless a predecessor daemon handed one
	// over (issue #73). Rulesets rarely change mid-monitoring; a consumer
	// that needs a refresh can restart.
	if p.key.kind == backend.KindPR && p.hub.rulesetFn != nil && p.ruleset == nil {
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
	// The tier transition detector starts from the full-monitoring baseline:
	// a first poll at the full tier announces nothing (nothing was shed), but
	// a first poll that already runs shed must say so loudly — a watcher that
	// never learns annotations were dropped would read their absence as
	// all-clear.
	p.tier = monitor.TierFull
	p.fetchOnce()
	delay := p.nextDelay()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-p.stopc:
			return
		case <-timer.C:
			if !p.timerPollPaused() {
				p.fetchOnce()
			}
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

// nextDelay returns the delay before the next timer tick, and — via the
// caller in run — whether that tick should fetch at all.
//
// The cadence is the jittered idle-interval for the current noChange count,
// stretched by the advisory GraphQL budget when the guard reports the budget
// low. Jitter de-phases pollers that subscribed at the same moment, so a
// fleet attaching many watchers at once does not burst requests in phase.
//
// Ceiling selection: idlePollCeiling when configured (WithIdleCeiling,
// issue #90), otherwise monitor.MaxIdleInterval; while the optional broker
// transport reports healthy (and an idle cap was configured via
// SetBrokerIdleCap) the extended cap wins instead — a real change now arrives
// as an immediate wake instead of waiting for the next tick, so the periodic
// poll only has to serve as a rare safety net. The moment the broker degrades,
// this reads the normal ceiling again on the very next call — no stale
// extended wait survives a lost connection.
//
// When pausing is armed (pollWhenBrokerHealthy: false) and the broker reports
// healthy, the tick carries no fetch: the returned delay shrinks to roughly
// the base interval so degradation is noticed within one tick, and run skips
// the fetch itself. Tier-shedding and budget-guard stretching continue to
// apply underneath whenever polling does run.
func (p *poller) nextDelay() time.Duration {
	p.mu.Lock()
	noChange := p.noChange
	interval := p.interval
	p.mu.Unlock()

	ceiling := p.hub.effectiveIdleCeiling()
	healthy, brokerCap := p.hub.BrokerHealthy()
	if healthy && brokerCap > 0 {
		ceiling = brokerCap
	}

	if p.timerPollPaused() {
		// Paused: no fetch rides on this tick, so keep it cheap and short —
		// just often enough to notice a degrade without the transition wake.
		return monitor.Jittered(interval)
	}

	d := monitor.Jittered(monitor.IdleIntervalCapped(interval, noChange, ceiling))
	// A run of consecutive fetch failures backs the cadence off exactly as
	// the in-process loops did: the error backoff dominates the idle backoff
	// until a fetch succeeds again, so a hard-down GitHub is polled at
	// doubling delays up to the same 300s ceiling rather than at full rate.
	p.mu.Lock()
	errBackoff := p.errBackoff
	p.mu.Unlock()
	if errBackoff > d {
		d = monitor.Jittered(errBackoff)
	}
	if p.budget != nil {
		d += p.budget.Stretch(time.Now()).Extra
		if d > ceiling {
			d = ceiling
		}
	}
	return d
}

// effectiveIdleCeiling returns the configured idle-backoff ceiling, or the
// monitor package's default when idlePollCeiling is unset (issue #90).
func (h *Hub) effectiveIdleCeiling() time.Duration {
	if h.idleCeiling > 0 {
		return h.idleCeiling
	}
	return monitor.MaxIdleInterval
}

// timerPollPaused reports whether timer-driven fetching is currently
// suspended: pauseWhenBrokerHealthy armed (pollWhenBrokerHealthy: false) and
// the broker transport reporting healthy. Wakes never pause — an event-driven
// refresh always fetches.
func (p *poller) timerPollPaused() bool {
	if !p.hub.pauseWhenBrokerHealthy {
		return false
	}
	healthy, _ := p.hub.BrokerHealthy()
	return healthy
}

func (p *poller) stop() {
	p.once.Do(func() { close(p.stopc) })
}

// fetchOnce reads the current fetch function under the hub lock, fetches, and
// fans the snapshot out to every subscriber's snapshotCh. Sends are
// non-blocking: a consumer that has not drained its previous snapshot drops
// the intermediate one — acceptable for a polling monitor, which only ever
// observes states at poll boundaries anyway.
func (p *poller) fetchOnce() {
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
			// Degrade loudly, once per episode: a fetch error must reach
			// subscribers, never vanish — a silently blind watcher reads as
			// "all clear". Consecutive identical failures are one episode,
			// one broadcast (issue #66); a changed error re-notifies, and the
			// recovery notice goes out on the next successful fetch. The
			// previous snapshot is retained; no inference replaces it.
			if p.enterDegraded("graphql", fmt.Sprintf("%v", err)) {
				p.broadcast(monitor.Event{
					Type:            monitor.EventDegraded,
					DegradedSurface: "graphql",
					DegradedMessage: fmt.Sprintf("%v", err),
				})
			}
			p.mu.Lock()
			p.errBackoff = monitor.NextErrBackoff(p.errBackoff, p.interval)
			p.mu.Unlock()
			return
		}
	}
	// A successful fetch ends every degraded episode this poller has in
	// flight: announce the recovery before the fresh snapshot, so a
	// consumer never reads the outage as ongoing past this point.
	for _, surface := range p.recoverDegraded() {
		p.broadcast(monitor.Event{
			Type:   monitor.EventDegraded,
			Notice: fmt.Sprintf("✅ API recovered (%s) on %s", surface, p.label()),
		})
	}
	p.mu.Lock()
	p.errBackoff = 0
	p.mu.Unlock()
	// Track fingerprint changes so run() can idle-back off a quiet target the
	// way the in-process loop does. Errors leave noChange untouched: a fetch
	// that failed tells us nothing about whether the target changed.
	prevFp := ""
	p.mu.Lock()
	if p.latest != nil {
		prevFp = p.traits.fingerprint(p.latest)
	}
	if p.traits.fingerprint(curr) != prevFp {
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

// enterDegraded records a degraded observation of the given API surface and
// reports whether it is new information: the surface just degraded, or it is
// degraded with a different message than the last broadcast. Consecutive
// identical failed fetches are one episode, one broadcast (issue #66) — the
// same transition-noticing semantics the in-process loops use.
func (p *poller) enterDegraded(surface, msg string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.degraded[surface] == msg {
		return false
	}
	if p.degraded == nil {
		p.degraded = make(map[string]string)
	}
	p.degraded[surface] = msg
	return true
}

// recoverDegraded returns the sorted surfaces currently in a degraded episode
// and clears them — the caller has just fetched successfully again.
// Deterministic order keeps multi-surface recoveries stable for log diffing.
func (p *poller) recoverDegraded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.degraded) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.degraded))
	for s := range p.degraded {
		out = append(out, s)
	}
	sort.Strings(out)
	p.degraded = nil
	return out
}

// selectTier returns the fetch tier for the next poll: TierFull when no
// BudgetGuard is wired, otherwise derived from the advisory GraphQL budget —
// clamped to what the kind's queries can actually shed. Ref/commit targets
// carry no comments or reviews, so their only meaningful step is dropping
// annotations (matching runRef); issue/run/repo queries have no tiers at all.
func (p *poller) selectTier() monitor.QueryTier {
	if !p.traits.tiered {
		return monitor.TierFull
	}
	tier := monitor.TierFull
	if p.budget != nil {
		if remaining, limit, ok := p.budget.GraphQLRemaining(time.Now()); ok {
			tier = monitor.TierForRemaining(remaining, limit)
		}
	}
	if p.key.kind == backend.KindRef || p.key.kind == backend.KindCommit {
		if tier == monitor.TierNoReviews || tier == monitor.TierStatus {
			tier = monitor.TierNoAnnotations
		}
	}
	return tier
}

// applyTier records a tier change. PR targets update every subscriber's
// snapshot options (so ShedSurfaces flow into their snapshots) and broadcast
// a loud notice naming what is no longer being watched (or that full
// monitoring has resumed). Ref/commit targets surface checks only, so — like
// runRef — the shed is logged, not notified. A watcher that quietly shed
// surfaces would turn a missing signal into an apparent all-clear.
func (p *poller) applyTier(tier monitor.QueryTier) {
	p.mu.Lock()
	p.tier = tier
	if p.key.kind == backend.KindPR {
		for s := range p.subs {
			s.snapOpts.Tier = tier
		}
	}
	p.mu.Unlock()

	if p.key.kind == backend.KindRef || p.key.kind == backend.KindCommit {
		if tier == monitor.TierNoAnnotations {
			fmt.Fprintf(os.Stderr, "gh-monitor: %s: graphql budget low, dropping annotations from polls\n",
				p.label())
		}
		return
	}

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
func (p *poller) broadcast(ev monitor.Event) {
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

// label renders the identity for notifications, in the same shape the
// renderer's degradedLabel uses for each kind.
func (p *poller) label() string {
	id := p.identity
	switch p.key.kind {
	case backend.KindRef:
		return fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, id.Ref)
	case backend.KindCommit:
		sha := id.CommitSHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		return fmt.Sprintf("%s/%s@%s", id.Owner, id.Repo, sha)
	case backend.KindRun:
		return fmt.Sprintf("%s/%s run #%d", id.Owner, id.Repo, id.RunID)
	case backend.KindRepo:
		return fmt.Sprintf("%s/%s", id.Owner, id.Repo)
	default:
		return fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, id.Number)
	}
}

// ---------------------------------------------------------------------------
// Subscriber
// ---------------------------------------------------------------------------

// sub is one consumer's handle: its own distillation (raw payload → status
// with this consumer's snapshot options), its own diff engine (a consume
// closure over one of the monitor package's per-kind consumers), a buffered
// snapshot inbox, a channel for loop-level notices (degraded / tier-shed),
// and an output channel of updates.
type sub struct {
	// mu guards handle — the one state mutated after creation (by loop's
	// Consume, and by ExportState's baseline read and takeResume's restore on
	// the handoff path).
	mu         sync.Mutex
	distill    func(raw any, snapOpts monitor.SnapshotOptions) backend.Status
	handle     consumerHandle
	cursorOf   func(raw any) string
	snapOpts   monitor.SnapshotOptions
	snapshotCh chan any
	notifCh    chan backend.Update
	out        chan backend.Update
	done       chan struct{}
	resumeID   string
	target     backend.Target
	watchOpts  backend.WatchOptions
}

// loop owns the consumer's diff goroutine. It reads raw payloads from the
// poller, distills each with the consumer's own SnapshotOptions (so
// IgnoredBots stay per-consumer), diffs against the consumer's own baseline,
// and emits notifications until done is closed or the target reaches a
// terminal state (merged/closed PR, closed issue, completed run), at which
// point it closes the output channel so daemon clients get a clean EOF.
func (s *sub) loop() {
	for {
		select {
		case raw, ok := <-s.snapshotCh:
			if !ok {
				close(s.out)
				return
			}
			emit := func(u backend.Update) {
				select {
				case s.out <- u:
				case <-s.done:
				}
			}
			if s.cursorOf != nil {
				cursor := s.cursorOf(raw)
				inner := emit
				emit = func(u backend.Update) {
					u.Cursor = cursor
					inner(u)
				}
			}
			curr := s.distill(raw, s.snapOpts)
			s.mu.Lock()
			terminal := s.handle.consume(curr, emit)
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
