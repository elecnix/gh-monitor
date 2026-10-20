package monitor

import "strings"

// allowlisted reports whether a value is in a set of names, matched
// case-insensitively after trimming surrounding whitespace.
//
// It is the one membership test the package's allowlist filters share. The
// filters differ in what a nil filter means and in what they compare — a
// check name, a notification kind, an annotation level — but the comparison
// itself is the same rule, and spelling it once keeps them from drifting:
// one of them trimming and lower-casing while the others did not would let
// the same name match one filter and miss another.
func allowlisted(allowed map[string]bool, value string) bool {
	return allowed[strings.ToLower(strings.TrimSpace(value))]
}
