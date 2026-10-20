package monitor

import (
	"sort"
	"strings"
)

// AnnotationChecks is a per-check-name allowlist for check-run annotations.
// A nil filter means "every check" (the default); a filter that allows
// nothing is the "none" case.
//
// Matching is exact and case-insensitive on the whole check name, because a
// check name is an opaque label ("dupdelta", "lint", "CI / test (node 20)")
// and a looser match would pull in the six workflow jobs that all carry the
// same deprecation warning — the noise this filter exists to remove.
type AnnotationChecks struct {
	allowed map[string]bool
	// none records the explicit "none" spelling, which wins over every
	// other name in the list.
	none bool
}

// NewAnnotationChecks builds an allowlist from the given check names. With no
// arguments it returns a non-nil filter that allows nothing (the "none"
// case). The names are trimmed and lower-cased, but NOT validated — use
// ParseAnnotationChecks for caller-facing input.
func NewAnnotationChecks(names ...string) *AnnotationChecks {
	f := &AnnotationChecks{allowed: make(map[string]bool, len(names))}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if n == "none" {
			f.none = true
			continue
		}
		f.allowed[n] = true
	}
	if f.none {
		// "none" means allow nothing, regardless of the other values.
		f.allowed = map[string]bool{}
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
func ParseAnnotationChecks(s string) (*AnnotationChecks, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	f := &AnnotationChecks{allowed: make(map[string]bool)}
	for _, raw := range strings.Split(s, ",") {
		n := strings.ToLower(strings.TrimSpace(raw))
		if n == "" {
			continue
		}
		if n == "none" {
			f.none = true
			continue
		}
		f.allowed[n] = true
	}
	if f.none {
		// "none" means allow nothing, regardless of the other values.
		f.allowed = map[string]bool{}
		return f, nil
	}
	if len(f.allowed) == 0 {
		// The input held nothing but separators: that names no check, so
		// it means "every check" rather than "no check at all".
		return nil, nil
	}
	return f, nil
}

// Allows reports whether a check name passes the filter. A nil filter allows
// every check. Matching is case-insensitive.
func (f *AnnotationChecks) Allows(checkName string) bool {
	if f == nil {
		return true
	}
	if f.none {
		return false
	}
	return f.allowed[strings.ToLower(strings.TrimSpace(checkName))]
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
	if a.none || len(a.allowed) == 0 {
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
	if a == nil {
		return false
	}
	return a.none || len(a.allowed) == 0
}

// String renders the allowlist for logs and help text.
func (a *AnnotationChecks) String() string {
	if a == nil {
		return "<all>"
	}
	if a.IsNone() {
		return "none"
	}
	return strings.Join(a.Names(), ",")
}
