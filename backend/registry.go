package backend

import (
	"errors"
	"fmt"
	"sort"
)

// ErrNoBackend is returned when no registered backend covers a target kind.
// It is deliberately an error rather than a silent no-op: a watcher with no
// source produces no events, which is indistinguishable from a quiet target.
var ErrNoBackend = errors.New("no backend registered")

// Registry resolves a Target to the backends that cover it.
//
// Registration is partial by design. A backend names the kinds it covers, and
// a kind-specific registration beats a catch-all one; among equally specific
// registrations the most recent wins. So a backend that handles only pull
// requests takes over pull requests and leaves every other kind to whoever
// registered for it — usually the built-in one.
//
// A Registry is not safe for concurrent registration. Build it during startup,
// then resolve from it.
type Registry struct {
	sources []registration[Source]
	readers []registration[Reader]
	pinned  string
}

type registration[T any] struct {
	name  string
	kinds map[Kind]bool // nil means every kind
	impl  T
}

func (r registration[T]) covers(k Kind) bool {
	return r.kinds == nil || r.kinds[k]
}

// specific reports whether the registration named its kinds rather than
// registering as a catch-all.
func (r registration[T]) specific() bool { return r.kinds != nil }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// RegisterSource adds a notification capability. A nil or empty kinds slice
// registers the source for every kind.
func (r *Registry) RegisterSource(name string, kinds []Kind, s Source) {
	r.sources = append(r.sources, registration[Source]{name: name, kinds: kindSet(kinds), impl: s})
}

// RegisterReader adds a query capability. A nil or empty kinds slice
// registers the reader for every kind.
func (r *Registry) RegisterReader(name string, kinds []Kind, rd Reader) {
	r.readers = append(r.readers, registration[Reader]{name: name, kinds: kindSet(kinds), impl: rd})
}

// Use registers a provider's capabilities.
func (r *Registry) Use(p Provider) error {
	if err := p.Register(r); err != nil {
		return fmt.Errorf("register backend %q: %w", p.Name(), err)
	}
	return nil
}

// Pin forces resolution to a single named backend. It fails when no backend
// of that name is registered, so a typo in --backend is caught at startup
// rather than silently falling back to the default.
func (r *Registry) Pin(name string) error {
	if name == "" {
		r.pinned = ""
		return nil
	}
	if !r.known(name) {
		return fmt.Errorf("unknown backend %q (registered: %s)", name, r.namesForError())
	}
	r.pinned = name
	return nil
}

// Pinned returns the pinned backend name, or "" when none is pinned.
func (r *Registry) Pinned() string { return r.pinned }

// SourceFor returns the Source covering t, and the name of the backend it
// came from.
func (r *Registry) SourceFor(t Target) (Source, string, error) {
	return resolve(r.sources, t.Kind, r.pinned, CapSource)
}

// ReaderFor returns the Reader covering t, and the name of the backend it
// came from.
func (r *Registry) ReaderFor(t Target) (Reader, string, error) {
	return resolve(r.readers, t.Kind, r.pinned, CapReader)
}

// resolve picks the registration for kind: the pinned backend when one is
// set, otherwise the most recent kind-specific match, otherwise the most
// recent catch-all.
func resolve[T any](regs []registration[T], kind Kind, pinned string, want Capability) (T, string, error) {
	var zero T
	if pinned != "" {
		for i := len(regs) - 1; i >= 0; i-- {
			if regs[i].name == pinned && regs[i].covers(kind) {
				return regs[i].impl, regs[i].name, nil
			}
		}
		return zero, "", fmt.Errorf("%w: backend %q does not provide a %s for %s targets",
			ErrNoBackend, pinned, want, kind)
	}
	for _, wantSpecific := range []bool{true, false} {
		for i := len(regs) - 1; i >= 0; i-- {
			if regs[i].specific() == wantSpecific && regs[i].covers(kind) {
				return regs[i].impl, regs[i].name, nil
			}
		}
	}
	return zero, "", fmt.Errorf("%w: no %s for %s targets", ErrNoBackend, want, kind)
}

// backendAcc accumulates the capabilities and kinds registered under one name.
type backendAcc struct {
	caps  map[Capability]bool
	kinds map[Kind]bool
	all   bool // a catch-all registration covers every kind
}

func (a *backendAcc) merge(kinds map[Kind]bool) {
	if kinds == nil {
		a.all = true
		return
	}
	for k := range kinds {
		a.kinds[k] = true
	}
}

// Info describes one registered backend for `gh monitor backends`.
type Info struct {
	Name string `json:"name"`
	// Capabilities are the surfaces this backend provides, in canonical order.
	Capabilities []Capability `json:"capabilities"`
	// Kinds are the target kinds it covers, in canonical order. Nil means
	// every kind.
	Kinds []Kind `json:"kinds,omitempty"`
}

// List returns every registered backend, sorted by name. A backend that
// registered several capabilities appears once, with all of them.
func (r *Registry) List() []Info {
	byName := map[string]*backendAcc{}
	get := func(name string) *backendAcc {
		a := byName[name]
		if a == nil {
			a = &backendAcc{caps: map[Capability]bool{}, kinds: map[Kind]bool{}}
			byName[name] = a
		}
		return a
	}
	for _, s := range r.sources {
		a := get(s.name)
		a.caps[CapSource] = true
		a.merge(s.kinds)
	}
	for _, rd := range r.readers {
		a := get(rd.name)
		a.caps[CapReader] = true
		a.merge(rd.kinds)
	}

	out := make([]Info, 0, len(byName))
	for name, a := range byName {
		info := Info{Name: name}
		for _, c := range []Capability{CapSource, CapReader} {
			if a.caps[c] {
				info.Capabilities = append(info.Capabilities, c)
			}
		}
		if !a.all {
			for _, k := range AllKinds() {
				if a.kinds[k] {
					info.Kinds = append(info.Kinds, k)
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) known(name string) bool {
	for _, s := range r.sources {
		if s.name == name {
			return true
		}
	}
	for _, rd := range r.readers {
		if rd.name == name {
			return true
		}
	}
	return false
}

func (r *Registry) namesForError() string {
	seen := map[string]bool{}
	var names []string
	for _, s := range r.sources {
		if !seen[s.name] {
			seen[s.name] = true
			names = append(names, s.name)
		}
	}
	for _, rd := range r.readers {
		if !seen[rd.name] {
			seen[rd.name] = true
			names = append(names, rd.name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}

// kindSet converts a kinds slice to a lookup set; nil/empty means every kind.
func kindSet(kinds []Kind) map[Kind]bool {
	if len(kinds) == 0 {
		return nil
	}
	set := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	return set
}
