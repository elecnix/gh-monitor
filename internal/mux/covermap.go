package mux

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend/remote"
)

// DefaultCoveredSafetyInterval is how long a covered repository may go
// without an event before the hub fetches an active watch on it once.
const DefaultCoveredSafetyInterval = 30 * time.Minute

// DefaultDegradedGrace is how long a sub-daemon may report itself degraded
// before its repositories stop counting as covered.
const DefaultDegradedGrace = 2 * time.Minute

// CoverageMap records which sub-daemon covers which repository, from the
// sub-daemons' own coverage streams. It answers one question for the router:
// does a webhook-driven source answer for this repository right now? A
// repository nobody has reported is uncovered, so the hub keeps polling it.
//
// The map persists across restarts (Save, Load) so a repository that was
// covered routes to its sub-daemon before the sub-daemon reports anything.
// A stale entry costs at most one safety interval of delay.
type CoverageMap struct {
	ttl   time.Duration
	grace time.Duration
	now   func() time.Time

	mu       sync.Mutex
	repos    map[string]coverEntry // lower-case "owner/repo"
	degraded map[string]time.Time  // daemon name -> when it began reporting degraded
	changed  chan struct{}         // closed and replaced on every change
	timers   map[string]*time.Timer
	epochs   map[string]uint64 // daemon name -> full sets received, see Epoch
}

type coverEntry struct {
	Daemon    string    `json:"daemon"`
	Evidence  time.Time `json:"evidence"`   // last time the daemon vouched for the repository
	LastEvent time.Time `json:"last_event"` // last event the daemon saw for it
}

// NewCoverageMap returns an empty map. ttl bounds how old a persisted entry
// may be when loaded; grace is how long a degraded sub-daemon keeps its
// repositories.
func NewCoverageMap(ttl, grace time.Duration) *CoverageMap {
	return &CoverageMap{
		ttl: ttl, grace: grace, now: time.Now,
		repos:    map[string]coverEntry{},
		degraded: map[string]time.Time{},
		changed:  make(chan struct{}),
		timers:   map[string]*time.Timer{},
		epochs:   map[string]uint64{},
	}
}

func repoKey(owner, repo string) string { return strings.ToLower(owner + "/" + repo) }

// Changed returns a channel closed on the next change. Take it before
// reading the state it guards, so a change in between is not missed.
func (m *CoverageMap) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *CoverageMap) bumpLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Retain forgets the repositories daemon covered that are not in keep. A
// stream calls it once its full set is complete. It starts a new epoch for
// the daemon, which wakes the routers even when no entry changed.
func (m *CoverageMap) Retain(daemon string, keep map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.repos {
		if e.Daemon == daemon && !keep[k] {
			delete(m.repos, k)
		}
	}
	m.setDegradedLocked(daemon, false)
	m.epochs[daemon]++
	m.bumpLocked()
}

// Epoch counts the full sets daemon has sent. A new one usually means the
// sub-daemon restarted, so a watch it dropped may go back to it.
func (m *CoverageMap) Epoch(daemon string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epochs[daemon]
}

// Known reports whether any daemon has reported owner/repo, whether or not
// that daemon is live or trusted right now.
func (m *CoverageMap) Known(owner, repo string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.repos[repoKey(owner, repo)]
	return ok
}

// Poke wakes everything waiting on Changed. The registry calls it when a
// sub-daemon appears or disappears, which moves watches without any entry
// changing.
func (m *CoverageMap) Poke() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bumpLocked()
}

// Apply records one coverage entry from a daemon. An entry with no repo
// reports the daemon's own health.
func (m *CoverageMap) Apply(daemon string, e remote.CoverageEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Repo == "" {
		m.setDegradedLocked(daemon, e.Degraded)
		m.bumpLocked()
		return
	}
	key := strings.ToLower(e.Repo)
	prev, had := m.repos[key]
	if !e.Covered {
		if had && prev.Daemon == daemon {
			delete(m.repos, key)
			m.bumpLocked()
		}
		return
	}
	next := coverEntry{Daemon: daemon, Evidence: m.now(), LastEvent: e.LastEvent}
	if had && prev.Daemon == daemon && next.LastEvent.IsZero() {
		next.LastEvent = prev.LastEvent
	}
	m.repos[key] = next
	// A refresh of an already covered repository moves no watch, so it does
	// not wake the routers; the safety timer reads LastEvent when it fires.
	if !had || prev.Daemon != daemon {
		m.bumpLocked()
	}
}

func (m *CoverageMap) setDegradedLocked(daemon string, degraded bool) {
	if !degraded {
		delete(m.degraded, daemon)
		if t := m.timers[daemon]; t != nil {
			t.Stop()
			delete(m.timers, daemon)
		}
		return
	}
	if _, already := m.degraded[daemon]; already {
		return
	}
	m.degraded[daemon] = m.now()
	// Degradation past the grace period is a change in its own right: wake
	// the routers when it lapses.
	m.timers[daemon] = time.AfterFunc(m.grace, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.bumpLocked()
	})
}

// Covered reports which daemon covers owner/repo and when it last saw an
// event there. A daemon that has been degraded for the grace period covers
// nothing.
func (m *CoverageMap) Covered(owner, repo string) (daemon string, lastEvent time.Time, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.repos[repoKey(owner, repo)]
	if !ok {
		return "", time.Time{}, false
	}
	if since, bad := m.degraded[e.Daemon]; bad && m.now().Sub(since) >= m.grace {
		return "", time.Time{}, false
	}
	last := e.LastEvent
	if last.IsZero() {
		last = e.Evidence
	}
	return e.Daemon, last, true
}

// Snapshot returns the covered repositories, sorted, for logs and tests.
func (m *CoverageMap) Snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.repos))
	for k := range m.repos {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// persisted is the file shape.
type persisted struct {
	Repos map[string]coverEntry `json:"repos"`
}

// Save writes the map atomically.
func (m *CoverageMap) Save(path string) error {
	m.mu.Lock()
	b, err := json.MarshalIndent(persisted{Repos: m.repos}, "", "  ")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".coverage-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load reads a saved map, dropping entries whose evidence is older than the
// TTL. A missing or unreadable file leaves the map empty, which fails
// towards polling.
func (m *CoverageMap) Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, e := range p.Repos {
		if m.ttl > 0 && now.Sub(e.Evidence) < m.ttl {
			m.repos[strings.ToLower(k)] = e
		}
	}
	m.bumpLocked()
	return nil
}

// Export and Import carry the map across the in-memory upgrade handoff.
func (m *CoverageMap) Export() json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := json.Marshal(persisted{Repos: m.repos})
	return b
}

func (m *CoverageMap) Import(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var p persisted
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, e := range p.Repos {
		if m.ttl > 0 && now.Sub(e.Evidence) < m.ttl {
			m.repos[strings.ToLower(k)] = e
		}
	}
	m.bumpLocked()
}
