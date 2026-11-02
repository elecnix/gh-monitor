package monitor

import (
	"sort"
	"strings"
)

// AnnotationChecks is a per-check-name allowlist for check-run annotations.
// A nil filter means "every check" (the default); a filter that holds no
// names allows nothing.
//
// Matching is exact and case-insensitive on the whole check name, because a
// check name is an opaque label ("dupdelta", "lint", "CI / test (node 20)")
// and a looser match would pull in the six workflow jobs that all carry the
// same deprecation warning — the noise this filter exists to remove.
type AnnotationChecks struct {
	allowed map[string]bool
	// excluded is the deny list. It is applied first and it wins: an operator
	// who writes "everything except dupdelta" means it even when dupdelta is
	// also on the allow list, because the exclusion is the more specific
	// statement — and because a deny that a stale allow can override is a deny
	// nobody can reason about.
	excluded map[string]bool
}

// NewAnnotationChecks builds an allowlist from the given check names. With no
// arguments it returns a non-nil filter that allows nothing (the "none"
// case). The names are trimmed and lower-cased, but NOT validated — use
// ParseAnnotationChecks for caller-facing input.
//
// The "none" spelling means "allow nothing", whichever other names
// accompany it: it is the explicit way to say so, and dropping it in
// favour of the other names would turn a deliberate mute back on.
func NewAnnotationChecks(names ...string) *AnnotationChecks {
	f := &AnnotationChecks{allowed: make(map[string]bool, len(names))}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		switch n {
		case "":
			continue
		case "none":
			f.allowed = map[string]bool{}
			return f
		}
		f.allowed[n] = true
	}
	return f
}

// ParseAnnotationChecks parses a comma-separated list of check names into an
// AnnotationChecks. Empty/blank entries are dropped, surrounding whitespace
// is trimmed, and matching is case-insensitive.
//
// An empty input string returns a nil filter — "every check" — because an
// empty list would otherwise read as "no annotations at all" and silently mute
// a watch the operator never asked to mute. "none" returns a filter that
// allows nothing, whichever other values accompany it; that is the spelling
// the filter's own Names() uses to cross a process boundary.
// NewAnnotationFilter builds a filter from an allow list and a deny list. An
// empty allow list means every check (a nil filter); a nil or empty deny list
// excludes nothing. The two lists compose: exclusion is applied first, so
// "all except lint" and "lint and test, except lint" both work.
func NewAnnotationFilter(allow, exclude []string) *AnnotationChecks {
	// An empty allow list means "every check", which only an absent allow map
	// can express: NewAnnotationChecks() with no names is the deliberate "none"
	// spelling, and the two must stay distinguishable.
	var f *AnnotationChecks
	if len(allow) > 0 {
		f = NewAnnotationChecks(allow...)
	}
	denied := map[string]bool{}
	for _, n := range exclude {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" {
			denied[n] = true
		}
	}
	if len(denied) > 0 {
		if f == nil {
			f = &AnnotationChecks{allowed: nil}
		}
		f.excluded = denied
	}
	return f
}

// ParseAnnotationExclude parses a comma-separated deny list. Empty input means
// "exclude nothing", and there is deliberately no "none" spelling here: the
// absence of an exclusion is the empty string, and a "none" that meant "exclude
// nothing" would be the mirror image of the allow list's "none" and confuse
// every reader of the two.
func ParseAnnotationExclude(s string) *AnnotationChecks {
	names := make([]string, 0, 4)
	for _, raw := range strings.Split(s, ",") {
		if n := strings.ToLower(strings.TrimSpace(raw)); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return NewAnnotationFilter(nil, names)
}

func ParseAnnotationChecks(s string) (*AnnotationChecks, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	names := make([]string, 0, 4)
	for _, raw := range strings.Split(s, ",") {
		if n := strings.ToLower(strings.TrimSpace(raw)); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		// The input held nothing but separators: that names no check, so
		// it means "every check" rather than "no check at all".
		return nil, nil
	}
	return NewAnnotationChecks(names...), nil
}

// Allows reports whether a check name passes the filter. A nil filter allows
// every check. Matching is case-insensitive.
func (f *AnnotationChecks) Allows(checkName string) bool {
	if f == nil {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(checkName))
	if f.excluded[name] {
		return false
	}
	if f.allowed == nil {
		// No allow list in force: only the deny list narrows anything.
		return true
	}
	return allowlisted(f.allowed, checkName)
}

// ExcludedNames returns the denied check names, sorted, so the filter can cross
// a process boundary alongside Names().
func (a *AnnotationChecks) ExcludedNames() []string {
	if a == nil || len(a.excluded) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.excluded))
	for n := range a.excluded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Unmatched returns the allow-listed names that match none of observed, sorted.
//
// This is the answer to a failure that is otherwise silent: a typo in a check
// name turns an allow list into a filter that matches nothing, and "no
// annotations arrived" is then indistinguishable from "your filter is wrong".
// An empty result means every allow-listed name was seen. A nil filter, or one
// that allows nothing deliberately ("none"), has nothing to match, so it reports
// nothing: a mute is not a typo.
func (a *AnnotationChecks) Unmatched(observed []string) []string {
	if a == nil || len(a.allowed) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(observed))
	for _, o := range observed {
		seen[strings.ToLower(strings.TrimSpace(o))] = true
	}
	var out []string
	for name := range a.allowed {
		if !seen[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Names returns the check names this filter allows, so the filter can cross
// a process boundary and be rebuilt with ParseAnnotationChecks.
//
// A nil filter returns nil, meaning "every check". A filter that allows
// nothing returns ["none"] — the spelling ParseAnnotationChecks understands —
// because an empty slice would otherwise read as "every check" and quietly
// turn the filter off. The names come back sorted, so the same filter
// always produces the same wire form.
func (a *AnnotationChecks) Names() []string {
	if a == nil {
		return nil
	}
	// A deny-only filter has no allow list at all: it means "every check except
	// these", so it serialises as nil — every check — and the exclusions travel
	// in ExcludedNames. Serialising it as ["none"] would cross the boundary as a
	// deliberate mute and silence the watch the operator just narrowed.
	if a.allowed == nil && len(a.excluded) > 0 {
		return nil
	}
	if len(a.allowed) == 0 {
		return []string{"none"}
	}
	out := make([]string, 0, len(a.allowed))
	for n := range a.allowed {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsNone reports whether the filter allows nothing — either because it was
// built with the "none" spelling, or because it holds no names at all. A nil
// filter is not "none": it allows every check.
func (a *AnnotationChecks) IsNone() bool {
	// A nil allow map means "every check"; an empty one is the deliberate mute.
	return a != nil && a.allowed != nil && len(a.allowed) == 0
}

// String renders the allowlist for logs and help text.
func (a *AnnotationChecks) String() string {
	if a == nil {
		return "<all>"
	}
	parts := []string{}
	if a.IsNone() {
		parts = append(parts, "none")
	} else if a.allowed == nil {
		parts = append(parts, "<all>")
	} else if len(a.allowed) > 0 {
		parts = append(parts, strings.Join(a.Names(), ","))
	} else {
		parts = append(parts, "<all>")
	}
	for _, n := range a.ExcludedNames() {
		parts = append(parts, "!"+n)
	}
	return strings.Join(parts, ",")
}
