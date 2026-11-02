package cmd

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openPRWithTwoAnnotatedChecks is a monitor GraphQL payload for an open PR
// carrying two check runs, each with one warning-level annotation: the check
// a consumer watches, and the noisy one that reports the same warning from
// every workflow job.
func openPRWithTwoAnnotatedChecks() obj {
	ann := func(path, title, message string) obj {
		return obj{
			"path":            path,
			"annotationLevel": "WARNING",
			"title":           title,
			"message":         message,
			"location":        obj{"start": obj{"line": 12}},
		}
	}
	return obj{
		"repository": obj{
			"pullRequest": obj{
				"state":     "OPEN",
				"merged":    false,
				"mergeable": "MERGEABLE",
				"commits": obj{"nodes": []interface{}{obj{"commit": obj{
					"oid": "abcdef1234",
					"checkSuites": obj{"nodes": []interface{}{obj{
						"app":        obj{"name": "CI"},
						"status":     "COMPLETED",
						"conclusion": "SUCCESS",
						"checkRuns": obj{"nodes": []interface{}{
							obj{
								"name":       "dupdelta",
								"status":     "COMPLETED",
								"conclusion": "SUCCESS",
								"annotations": obj{"totalCount": 1, "nodes": []interface{}{
									ann("src/dup.go", "duplication", "clone of src/orig.go"),
								}},
							},
							obj{
								"name":       "lint",
								"status":     "COMPLETED",
								"conclusion": "SUCCESS",
								"annotations": obj{"totalCount": 1, "nodes": []interface{}{
									ann("src/x.go", "deprecated API", "Foo is deprecated, use Bar"),
								}},
							},
						}},
					}}},
				}}}},
			},
		},
	}
}

// annotationCheckNames returns the check names of the annotations in the
// emitted NDJSON.
//
// The result is SORTED: the annotations reach the snapshot in whatever order
// the check runs were walked, so an ordered comparison would fail on some
// runs and not others.
func annotationCheckNames(t *testing.T, stdout string) []string {
	t.Helper()
	var names []string
	for _, ln := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if ln == "" {
			continue
		}
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		if n["type"] != "check-annotations" {
			continue
		}
		detail, _ := n["detail"].(string)
		for _, line := range strings.Split(detail, "\n") {
			if !strings.HasPrefix(line, "dupdelta") && !strings.HasPrefix(line, "lint") {
				continue
			}
			names = append(names, strings.Fields(line)[0])
		}
	}
	sort.Strings(names)
	return names
}

// runMonitorOnceWith runs a one-shot watch against a PR carrying the two
// annotated checks, with the given extra flags, and returns the stdout it
// produced.
func runMonitorOnceWith(t *testing.T, extraArgs ...string) string {
	t.Helper()
	stdout, _ := runMonitorCommand(t, "", openPRWithTwoAnnotatedChecks(), extraArgs...)
	return stdout
}

// runMonitorOnceWithPrefs is runMonitorOnceWith with a preferences file in
// place, written the way `gh monitor prefs set` would.
func runMonitorOnceWithPrefs(t *testing.T, prefsJSON string, extraArgs ...string) string {
	t.Helper()
	stdout, _ := runMonitorCommand(t, prefsJSON, openPRWithTwoAnnotatedChecks(), extraArgs...)
	return stdout
}

// TestMonitorOnceAnnotationChecksFilter confirms the --annotation-checks flag
// narrows the reported annotations to the named checks (issue #110).
//
// The case it exists for: a PR carries the same deprecation warning from
// every workflow job plus one finding from the check the consumer actually
// watches. Without the filter the warning buries the finding.
func TestMonitorOnceAnnotationChecksFilter(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	t.Run("without the filter every check is reported", func(t *testing.T) {
		// The two checks, in sorted order.
		assert.Equal(t, []string{"dupdelta", "lint"}, annotationCheckNames(t, runMonitorOnceWith(t)))
	})

	t.Run("the flag keeps only the named check", func(t *testing.T) {
		names := annotationCheckNames(t, runMonitorOnceWith(t, "--annotation-checks", "dupdelta"))
		require.NotEmpty(t, names, "the named check's annotation must be reported")
		for _, n := range names {
			assert.Equal(t, "dupdelta", n, "--annotation-checks must drop the other checks' annotations")
		}
	})

	t.Run("none drops every annotation", func(t *testing.T) {
		names := annotationCheckNames(t, runMonitorOnceWith(t, "--annotation-checks", "none"))
		assert.Empty(t, names, "\"none\" must drop every annotation")
	})
}

// TestMonitorOnceAnnotationChecksReadsPreferences confirms the annotationChecks
// preference applies when the flag is absent, and that the flag overrides it.
func TestMonitorOnceAnnotationChecksReadsPreferences(t *testing.T) {
	// The stored list, written the way `gh monitor prefs set` would.
	const stored = `{"annotationChecks": ["dupdelta"]}`

	// The stored list applies.
	names := annotationCheckNames(t, runMonitorOnceWithPrefs(t, stored))
	require.NotEmpty(t, names, "the configured check's annotation must be reported")
	for _, n := range names {
		assert.Equal(t, "dupdelta", n, "the configured allowlist must filter the annotations")
	}

	// The flag overrides the stored value: an explicit per-invocation
	// allowlist replaces the stored one, so an agent can ask for a
	// different check for one probe.
	names = annotationCheckNames(t, runMonitorOnceWithPrefs(t, stored, "--annotation-checks", "lint"))
	require.NotEmpty(t, names, "the flagged check's annotation must be reported")
	for _, n := range names {
		assert.Equal(t, "lint", n, "the flag must override the stored allowlist")
	}
}
