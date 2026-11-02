package cmd

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/handoff"
)

// clientRegistry records the connections the daemon is serving, so the daemon
// can say what it is serving and `gh monitor doctor` can compare that against
// what is running on the machine (issue #193).
//
// It is deliberately a record, not a limit: nothing here refuses, throttles,
// or times out a client. Its value is that "the daemon is serving this" stops
// being an assumption and becomes a fact an operator can read.
type clientRegistry struct {
	mu       sync.Mutex
	entries  map[*clientEntry]struct{}
	sequence int64
}

// clientEntry is one connection. The fields that come from the client's own
// requests are guarded by the registry's mutex, because a serving goroutine
// writes them while an OpInfo request reads every entry.
type clientEntry struct {
	pid   int
	since time.Time
	seq   int64

	resumes []string
	targets []backend.Target
	watch   bool
}

// clientEntryKey carries the requesting connection's own entry in its
// context, so a report about clients can leave its author out.
type clientEntryKey struct{}

func withClientEntry(ctx context.Context, e *clientEntry) context.Context {
	if e == nil {
		return ctx
	}
	return context.WithValue(ctx, clientEntryKey{}, e)
}

func clientEntryFrom(ctx context.Context) *clientEntry {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(clientEntryKey{}).(*clientEntry)
	return e
}

func newClientRegistry() *clientRegistry {
	return &clientRegistry{entries: make(map[*clientEntry]struct{})}
}

// add registers a connection and returns its entry. A nil registry accepts
// registrations and reports nothing, so a daemon built without one (as in the
// existing tests) behaves exactly as before.
func (r *clientRegistry) add(pid int) *clientEntry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	e := &clientEntry{pid: pid, since: time.Now(), seq: r.sequence}
	r.entries[e] = struct{}{}
	return e
}

// remove forgets a connection that has ended.
func (r *clientRegistry) remove(e *clientEntry) {
	if r == nil || e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, e)
}

// observe records what a connection asked for.
func (r *clientRegistry) observe(e *clientEntry, opts backend.WatchOptions, t backend.Target) {
	// Guard both receivers: an entry from a nil registry is nil, and
	// observe on it must be as harmless as add was.
	if r == nil || e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e.watch = e.watch || !opts.Once
	if opts.ResumeID != "" {
		e.resumes = appendUniq(e.resumes, opts.ResumeID)
	}
	for _, have := range e.targets {
		if have == t {
			return
		}
	}
	e.targets = append(e.targets, t)
}

// snapshot returns the connected clients, in connection order so the output
// of one daemon start is stable. Except names a connection to leave out — the
// one asking, which would otherwise always list itself.
func (r *clientRegistry) snapshotExcept(except *clientEntry) []handoff.ClientInfo {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]handoff.ClientInfo, 0, len(r.entries))
	for e := range r.entries {
		if e == except {
			continue
		}
		out = append(out, handoff.ClientInfo{
			PID:     e.pid,
			Resumes: append([]string(nil), e.resumes...),
			Targets: append([]backend.Target(nil), e.targets...),
			Watch:   e.watch,
			Since:   e.since,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// appendUniq appends s unless it is already present.
func appendUniq(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}
