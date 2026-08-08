package monitor

import (
	"fmt"
	"strings"
)

// AnnotationLevels is a per-level allowlist for check-run annotations. A nil
// filter means "default" (warning + failure); an empty allowlist is the "none"
// case (allow nothing). Matching is case-insensitive.
type AnnotationLevels struct {
	allowed map[string]bool
}

// defaultAnnotationLevels is the nil-filter fallback: warning + failure,
// normalised the same way the configured path normalises input.
var defaultAnnotationLevels = NewAnnotationLevels("warning", "failure")

// validAnnotationLevels is the set of recognised annotation level strings.
func validAnnotationLevels() map[string]bool {
	return map[string]bool{
		"notice":  true,
		"warning": true,
		"failure": true,
		"none":    true,
	}
}

// NewAnnotationLevels builds an allowlist from the given level strings. With
// no arguments it returns a non-nil filter that allows nothing (the "none"
// case). The levels are normalised (trimmed, lower-cased) but NOT validated
// against the known set — use ParseAnnotationLevels for caller-facing input
// that must reject typos.
func NewAnnotationLevels(levels ...string) *AnnotationLevels {
	f := &AnnotationLevels{allowed: make(map[string]bool, len(levels))}
	for _, l := range levels {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			f.allowed[l] = true
		}
	}
	return f
}

// ParseAnnotationLevels parses a comma-separated list of annotation levels
// into an AnnotationLevels, rejecting any level that is not recognised.
// Empty/blank entries are dropped, surrounding whitespace is trimmed, and
// matching is case-insensitive. An empty input string returns the default
// (warning + failure). "none" returns a filter that allows nothing.
func ParseAnnotationLevels(s string) (*AnnotationLevels, error) {
	valid := validAnnotationLevels()
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return NewAnnotationLevels("warning", "failure"), nil
	}

	parts := strings.Split(trimmed, ",")
	f := &AnnotationLevels{allowed: make(map[string]bool, len(parts))}
	hasNone := false
	for _, raw := range parts {
		l := strings.ToLower(strings.TrimSpace(raw))
		if l == "" {
			continue
		}
		if !valid[l] {
			return nil, fmt.Errorf("unknown annotation level %q (expected one of: notice, warning, failure, none)", l)
		}
		if l == "none" {
			hasNone = true
			continue
		}
		f.allowed[l] = true
	}
	if hasNone {
		// "none" means allow nothing, regardless of other values.
		f.allowed = map[string]bool{}
	}
	return f, nil
}

// Allows reports whether a level passes the filter. A nil filter uses the
// default (warning + failure). Matching is case-insensitive.
func (f *AnnotationLevels) Allows(level string) bool {
	if f == nil {
		return defaultAnnotationLevels.Allows(level)
	}
	return f.allowed[strings.ToLower(strings.TrimSpace(level))]
}

