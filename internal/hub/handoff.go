// Handoff state: what one daemon's hub knows that the next daemon needs in
// order to take over watching the same targets without a replay storm or a
// blind gap (issue #73). The state travels in memory — over the daemon
// socket, as the handoff request's response payload — and is never written
// to a file.
//
// Two kinds of state move across:
//
//   - Pollers: one per watched identity (any target kind), carrying the last
//     fetched raw snapshot, the idle-backoff counter, the query tier, and the
//     cached ruleset. The next daemon seeds its pollers with these, so a
//     watcher that reconnects is served from continuity, not from a cold
//     first poll.
//   - Resumes: one per connected watcher, carrying the distilled baseline
//     that watcher had already been shown. The next daemon holds these until
//     the watcher reconnects with the same ResumeID, then diffs against the
//     carried baseline — so the handoff replays nothing and misses nothing.
package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// StateVersion is the schema version of State. A daemon asked to adopt a
// state it does not understand refuses rather than guesses.
const StateVersion = 1

// resumeTTL bounds how long the next daemon holds a watcher's baseline for a
// reconnect that never comes. A watcher reconnects within seconds of a
// handoff; ten minutes covers pathological scheduling many times over, after
// which the entry is simply dropped — a late reconnect then gets a fresh
// first poll, which is the pre-handoff behaviour anyway. A variable so tests
// can shorten it.
var resumeTTL = 10 * time.Minute

// State is the hub's transferable knowledge, in schema version Version.
type State struct {
	Version int           `json:"version"`
	Pollers []PollerState `json:"pollers,omitempty"`
	Resumes []ResumeState `json:"resumes,omitempty"`
}

// PollerState is one watched identity's continuity. Latest is the raw fetch
// payload as JSON — its concrete shape depends on the identity's target kind,
// and the adopting daemon decodes it with that kind's traits rather than
// guessing.
type PollerState struct {
	Identity resolver.Identity      `json:"identity"`
	Latest   json.RawMessage        `json:"latest,omitempty"`
	NoChange int                    `json:"noChange,omitempty"`
	Tier     monitor.QueryTier      `json:"tier,omitempty"`
	Ruleset  *monitor.RulesetChecks `json:"ruleset,omitempty"`
}

// ResumeState is one connected watcher's continuity: where its stream was, and
// what it had already been shown. Baseline is the watcher's distilled status
// as JSON — decoded on the adopting side by the target's kind.
type ResumeState struct {
	ResumeID string               `json:"resumeId"`
	Target   backend.Target       `json:"target"`
	Options  backend.WatchOptions `json:"options"`
	Baseline json.RawMessage      `json:"baseline,omitempty"`
}

// resumeEntry is a ResumeState plus its expiry.
type resumeEntry struct {
	state   ResumeState
	expires time.Time
}

// ExportState snapshots everything a successor daemon needs. It is safe to
// call while pollers and subscribers run: each piece is read under the lock
// that owns it.
func (h *Hub) ExportState() State {
	h.mu.Lock()
	pollers := make([]*poller, 0, len(h.pollers))
	for _, p := range h.pollers {
		pollers = append(pollers, p)
	}
	resumes := make([]ResumeState, 0, len(h.resumes))
	for _, e := range h.resumes {
		resumes = append(resumes, e.state)
	}
	h.mu.Unlock()

	state := State{Version: StateVersion}
	for _, p := range pollers {
		ps := PollerState{Identity: p.identity}
		p.mu.Lock()
		if p.latest != nil {
			// The raw payload's concrete type is JSON-tagged, so it travels
			// encoded and the successor decodes it by kind.
			if b, err := json.Marshal(p.latest); err == nil {
				ps.Latest = b
			}
		}
		ps.NoChange, ps.Tier, ps.Ruleset = p.noChange, p.tier, p.ruleset
		subs := make([]*sub, 0, len(p.subs))
		for s := range p.subs {
			subs = append(subs, s)
		}
		p.mu.Unlock()

		for _, s := range subs {
			if s.resumeID == "" {
				continue
			}
			s.mu.Lock()
			st := s.handle.baseline()
			rs := ResumeState{
				ResumeID: s.resumeID,
				Target:   s.target,
				Options:  s.watchOpts,
			}
			s.mu.Unlock()
			if st != nil {
				if b, err := json.Marshal(st); err == nil {
					rs.Baseline = b
				}
			}
			state.Resumes = append(state.Resumes, rs)
		}
		state.Pollers = append(state.Pollers, ps)
	}
	// Baselines still held for watchers that have not reconnected yet travel
	// too — a successor that is itself replaced mid-handoff must not drop
	// them.
	state.Resumes = append(state.Resumes, resumes...)
	return state
}

// RestoreState seeds the hub with a predecessor's state: pollers are created
// (not started — a poller starts when its first subscriber arrives, exactly
// as a fresh one would) and resume entries are held for reconnecting
// watchers. An unrecognised schema version is an error, not a guess.
func (h *Hub) RestoreState(s State) error {
	if s.Version != StateVersion {
		return fmt.Errorf("handoff state version %d, this build understands %d", s.Version, StateVersion)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ps := range s.Pollers {
		key := keyOf(ps.Identity)
		if _, exists := h.pollers[key]; exists {
			continue // never clobber a poller that already started
		}
		p := newPoller(h, ps.Identity, h.interval)
		if len(ps.Latest) > 0 {
			raw := rawForKind(key.kind)
			if err := json.Unmarshal(ps.Latest, raw); err == nil {
				p.latest = raw
			}
		}
		p.noChange, p.tier, p.ruleset = ps.NoChange, ps.Tier, ps.Ruleset
		h.pollers[key] = p
	}
	now := time.Now()
	for _, rs := range s.Resumes {
		if rs.ResumeID == "" {
			continue
		}
		h.resumes[rs.ResumeID] = resumeEntry{state: rs, expires: now.Add(resumeTTL)}
	}
	return nil
}

// takeResume consumes the baseline held for resumeID, if one is live. Called
// with h.mu held.
func (h *Hub) takeResume(resumeID string) (json.RawMessage, bool) {
	entry, ok := h.resumes[resumeID]
	if !ok {
		return nil, false
	}
	delete(h.resumes, resumeID)
	if time.Now().After(entry.expires) || len(entry.state.Baseline) == 0 {
		return nil, false
	}
	return entry.state.Baseline, true
}
