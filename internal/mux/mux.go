// Package mux lets gh-monitor own one daemon socket while configured
// sub-daemons serve their own target kinds through it (issue #88).
//
// Before this package, a daemon with sub-daemons configured conceded the
// public socket entirely: whichever sub-daemon bound $GH_MONITOR_SOCK served
// only its own kinds, and every other target kind (workflow runs, whole
// repositories, issues) had no serving path at all. The fix inverts the
// ownership: gh-monitor always binds the public socket, each sub-daemon is
// launched against its own private socket (see SocketPath), and a Registry
// discovers what each live sub-daemon serves by reading its protocol hello.
// A RoutingSource then sends each watch to the sub-daemon that owns the
// target's kind and to the polling hub otherwise — so both event-driven
// sources and polled kinds coexist behind one socket.
//
// The sub-daemon side needs no changes: a sub-daemon already binds
// $GH_MONITOR_SOCK, so pointing that variable at a private path redirects it.
package mux

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
)

// ProbeInterval is how often the registry re-probes tracked sub-daemon
// sockets when Run drives it. Children take a moment to boot after a launch
// or a supervised restart, so discovery must keep trying; probing a dead path
// fails fast. A variable so tests can shorten it.
var ProbeInterval = 2 * time.Second

// SocketPath returns the private Unix socket path for one sub-daemon entry:
// <dir>/subdaemon-<name>.sock with the name sanitized to filesystem-safe
// characters. Deterministic, so the daemon and any operator tooling agree on
// where a given entry binds. A name that sanitizes to nothing still yields a
// usable .sock path rather than an error — config validation is the launcher's
// job, not the socket naming's.
func SocketPath(dir, name string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	if strings.Trim(clean, "-") == "" {
		clean = "subdaemon"
	}
	return filepath.Join(dir, "subdaemon-"+clean+".sock")
}

// Registry tracks configured sub-daemons, probes their private sockets to
// discover which target kinds each serves, and remembers the live ones.
type Registry struct {
	out io.Writer // lifecycle logging; never nil

	mu      sync.RWMutex
	tracks  map[string]remote.Transport // entry name -> how to reach it
	live    map[string]*remote.Provider // entry name -> last successful hello
	serving map[string][]backend.Kind   // entry name -> kinds announced (for change logs)
}

// NewRegistry builds a registry that logs discovery transitions to out.
func NewRegistry(out io.Writer) *Registry {
	if out == nil {
		out = io.Discard
	}
	return &Registry{
		out:     out,
		tracks:  map[string]remote.Transport{},
		live:    map[string]*remote.Provider{},
		serving: map[string][]backend.Kind{},
	}
}

// Track registers one sub-daemon entry for probing. Track is safe to call
// before or between Runs; later Probes pick up new entries.
func (r *Registry) Track(name string, tr remote.Transport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracks[name] = tr
}

// Probe runs one discovery pass over every tracked entry. An entry whose
// socket answers joins the live set; an entry that has gone quiet leaves it.
// Only transitions are logged, so a permanently dead entry costs one stat per
// pass and no noise.
func (r *Registry) Probe(ctx context.Context) {
	r.mu.RLock()
	tracks := make(map[string]remote.Transport, len(r.tracks))
	for n, tr := range r.tracks {
		tracks[n] = tr
	}
	r.mu.RUnlock()

	for name, tr := range tracks {
		prov, err := remote.Connect(ctx, tr)
		if err != nil {
			r.mu.Lock()
			_, wasLive := r.live[name]
			if wasLive {
				delete(r.live, name)
				delete(r.serving, name)
			}
			r.mu.Unlock()
			if wasLive {
				_, _ = fmt.Fprintf(r.out, "gh-monitor daemon: sub-daemon %q stopped answering; its kinds fall back to the polling hub\n", name)
			}
			continue
		}
		kinds := prov.Kinds()
		r.mu.Lock()
		prev, wasLive := r.serving[name]
		r.live[name] = prov
		r.serving[name] = kinds
		r.mu.Unlock()
		changed := !sameKinds(prev, kinds)
		if !wasLive || changed {
			_, _ = fmt.Fprintf(r.out, "gh-monitor daemon: sub-daemon %q (%s) serves %s — watches for those kinds route to it\n",
				name, prov.Name(), renderKinds(kinds))
		}
	}
}

// Run probes on a ticker until ctx is done. It exists so discovery survives
// supervised restarts without any caller having to poll by hand.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Probe(ctx)
		}
	}
}

// Provider returns the live provider that serves kind, or nil when no
// discovered sub-daemon covers it. When several do, the first tracked wins —
// deterministic, and irrelevant in practice since two sub-daemons serving the
// same kind is an operator misconfiguration.
func (r *Registry) Provider(kind backend.Kind) *remote.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tracks))
	for n := range r.tracks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p, ok := r.live[n]
		if !ok {
			continue
		}
		for _, k := range p.Kinds() {
			if k == kind {
				return p
			}
		}
	}
	return nil
}

// Kinds returns the union of kinds the currently live sub-daemons serve.
func (r *Registry) Kinds() []backend.Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []backend.Kind
	seen := map[backend.Kind]bool{}
	for _, ks := range r.serving {
		for _, k := range ks {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sameKinds compares two kind lists order-insensitively.
func sameKinds(a, b []backend.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[backend.Kind]bool, len(a))
	for _, k := range a {
		set[k] = true
	}
	for _, k := range b {
		if !set[k] {
			return false
		}
	}
	return true
}

// renderKinds formats a kind list for logs; nil means "every kind".
func renderKinds(kinds []backend.Kind) string {
	if len(kinds) == 0 {
		return "every kind"
	}
	quoted := make([]string, len(kinds))
	for i, k := range kinds {
		quoted[i] = string(k)
	}
	return strings.Join(quoted, ", ")
}
