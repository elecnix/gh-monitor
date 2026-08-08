// Package prefs is the user-overridable notification-template and preferences
// system. It resolves a JSON preferences file under the XDG config dir, merges
// it over authoritative defaults, and interpolates a fixed set of tokens into
// each event's template string.
//
// It is intentionally decoupled from internal/monitor: the template keys match
// the monitor's Event.Type strings (plus two loop-level keys) by convention, so
// a later PR can map an Event.Type to its template by name without this package
// importing monitor. This package is pure config + string templating.
//
// Ported from the pi-ghpr-monitor TypeScript extension (preferences.ts). The
// disableMergeTool / LLM-tool / merge-tool concepts are intentionally dropped:
// there is no LLM tool in a CLI.
package prefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Preferences holds the user's notification templates keyed by event kind,
// plus non-template configuration.
//
// Templates is keyed by the event kinds in DefaultPreferences. The two
// non-template fields (IgnoredBots, RetriggerComments) are plain config and are
// exempt from the template validation guardrail.
type Preferences struct {
	Templates         map[string]string `json:"templates"`
	IgnoredBots       []string          `json:"ignoredBots"`
	RetriggerComments bool              `json:"retriggerComments"`
}

// templateKeys are the exact, authoritative event-kind keys. They intentionally
// match the monitor's Event.Type strings plus the two loop-level keys
// (first-poll, all-clear) so a later PR maps Event.Type -> template by name.
var defaultTemplates = map[string]string{
	"new-unresolved-threads":   "💬 {unresolvedThreads} unresolved review thread(s) on {prLabel}",
	"new-general-comments":     "💭 {generalComments} new general comment(s) on {prLabel}",
	"conflict":                 "⚠️  Merge conflicts detected on {prLabel}. If CI is failing, resolve the conflict first — it may be causing the failures.",
	"new-failing-checks":       "❌ Failing CI checks on {prLabel}: {failingChecks}",
	"ci-all-green":             "✅ All CI checks passed on {prLabel}",
	"review-approved":          "✅ {prLabel} was approved by {reviewAuthor}",
	"review-changes-requested": "⛔ {reviewAuthor} requested changes on {prLabel}",
	"review-dismissed":         "🔄 Review dismissed on {prLabel}",
	"new-commit":               "📝 New commit {commitShortOid} pushed to {prLabel} by {commitAuthor}. Review the PR description to ensure it still reflects the latest changes.",
	"merged":                   "🔀 PR {prLabel} was merged. Monitoring stopped.",
	"closed":                   "❌ PR {prLabel} was closed. Monitoring stopped.",
	"first-poll":               "📡 Monitoring {prLabel} (polling every {intervalSec}s)",
	"all-clear":                "✨ {prLabel} — open, all clear",
	"issue-closed":             "❌ Issue {prLabel} was closed. Monitoring stopped.",
	"issue-reopened":           "🔄 Issue {prLabel} was reopened.",
	"issue-new-comment":        "💭 New comment on issue {prLabel}",
	"issue-mention":            "👋 You were mentioned on issue {prLabel}",

	// Workflow-run monitoring
	"run-queued":      "⏸️ Workflow run {runName} #{runNumber} on {owner}/{repo} is queued",
	"run-in-progress": "⏳ Workflow run {runName} #{runNumber} on {owner}/{repo} is now running",
	"run-completed":   "🏁 Workflow run {runName} #{runNumber} on {owner}/{repo} finished: {runConclusion}",

	// Repo monitoring
	"repo-new-pr":    "🆕 New PR {repoItemNumber}: {repoItemTitle} by {repoItemAuthor} in {prLabel}",
	"repo-new-issue": "🆕 New issue {repoItemNumber}: {repoItemTitle} by {repoItemAuthor} in {prLabel}",

	// Check-run annotations
	"check-annotations": "📋 {annotationCount}{annotationTruncated} annotation(s) from {annotationCheckNames} on {prLabel}",
}

// DefaultPreferences returns a fresh copy of the built-in defaults.
func DefaultPreferences() Preferences {
	templates := make(map[string]string, len(defaultTemplates))
	for k, v := range defaultTemplates {
		templates[k] = v
	}
	return Preferences{
		Templates:         templates,
		IgnoredBots:       []string{},
		RetriggerComments: false,
	}
}

// recognizedTokens is the fixed set of tokens Interpolate will replace. Any
// other {token} is left literal.
var recognizedTokens = map[string]bool{
	"owner":                 true,
	"repo":                  true,
	"number":                true,
	"host":                  true,
	"prLabel":               true,
	"prUrl":                 true,
	"unresolvedThreads":     true,
	"generalComments":       true,
	"failingChecks":         true,
	"conflict":              true,
	"intervalSec":           true,
	"reviewAuthor":          true,
	"commitOid":             true,
	"commitShortOid":        true,
	"commitUrl":             true,
	"commitAuthor":          true,
	"commitCoauthors":       true,
	"commitMessageHeadline": true,
	"issueState":            true,
	"issueTitle":            true,
	"issueComments":         true,

	"runId":         true,
	"runName":       true,
	"runNumber":     true,
	"runEvent":      true,
	"runStatus":     true,
	"runConclusion": true,
	"runBranch":     true,
	"runUrl":        true,

	"repoPRs":        true,
	"repoIssues":     true,
	"repoItemNumber": true,
	"repoItemTitle":  true,
	"repoItemAuthor": true,
	"repoItemUrl":    true,

	"annotationCount":      true,
	"annotationCheckNames": true,
	"annotationTruncated":  true,
	"annotationUrl":        true,
}

// tokenRE matches a single {token} placeholder. The token name is captured.
var tokenRE = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// Interpolate replaces each recognized {token} with vars[token]. Unrecognized
// tokens, and recognized tokens absent from vars, are left literally in place.
func Interpolate(template string, vars map[string]string) string {
	return tokenRE.ReplaceAllStringFunc(template, func(match string) string {
		name := match[1 : len(match)-1] // strip { }
		if !recognizedTokens[name] {
			return match
		}
		val, ok := vars[name]
		if !ok {
			return match
		}
		return val
	})
}

// hasToken reports whether s contains at least one {…} placeholder.
func hasToken(s string) bool {
	return tokenRE.MatchString(s)
}

// Validate rejects unknown template keys and any template value with no {…}
// token (pi's safety guardrail against a template that can never interpolate).
// The non-template config (IgnoredBots, RetriggerComments) is exempt.
func Validate(p Preferences) error {
	for key, tmpl := range p.Templates {
		if _, ok := defaultTemplates[key]; !ok {
			return fmt.Errorf("unknown template key: %q", key)
		}
		if !hasToken(tmpl) {
			return fmt.Errorf("template %q has no {token}: %q", key, tmpl)
		}
	}
	return nil
}

// ConfigPath resolves the preferences file path. When baseDir is non-empty it
// is used as the config base (for tests); otherwise XDG_CONFIG_HOME is used,
// falling back to $HOME/.config.
func ConfigPath(baseDir string) (string, error) {
	dir, err := configDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preferences.json"), nil
}

// configDir returns the gh-monitor config directory.
func configDir(baseDir string) (string, error) {
	base := baseDir
	if base == "" {
		base = os.Getenv("XDG_CONFIG_HOME")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "gh-monitor"), nil
}

// legacyConfigDir returns the old gh-pr-monitor config directory for migration.
func legacyConfigDir(baseDir string) (string, error) {
	base := baseDir
	if base == "" {
		base = os.Getenv("XDG_CONFIG_HOME")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "gh-pr-monitor"), nil
}

// resolvePath tries the new config path first, then falls back to the legacy
// path. If the legacy path is used, a one-time warning is printed to stderr.
func resolvePath(baseDir string) (string, error) {
	path, err := ConfigPath(baseDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	legacy, err := legacyPath(baseDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(legacy); err == nil {
		fmt.Fprintf(os.Stderr, "gh-monitor: using legacy config at %s; move to %s to silence this warning\n", legacy, path)
		return legacy, nil
	}

	return path, nil
}

// legacyPath returns the path to the old gh-pr-monitor preferences file.
func legacyPath(baseDir string) (string, error) {
	dir, err := legacyConfigDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preferences.json"), nil
}

// storedPreferences is the on-disk shape. Templates uses *string values so a
// JSON null is distinguishable from an absent key: null resets that key to
// default, absent leaves the default untouched.
type storedPreferences struct {
	Templates         map[string]*string `json:"templates"`
	IgnoredBots       []string           `json:"ignoredBots"`
	RetriggerComments *bool              `json:"retriggerComments,omitempty"`
}

// Load starts from DefaultPreferences and overlays the JSON file if present.
//
// Overlay rules:
//   - A stored template value that fails the templateless guardrail (no {…})
//     is dropped with a WARN to stderr, keeping the default.
//   - A stored JSON null for a template key resets that key to its default.
//   - A missing file returns the defaults with no error.
func Load(baseDir string) (Preferences, error) {
	stored, err := loadStored(baseDir)
	if err != nil {
		// loadStored already returns an empty shape on a missing file, but a
		// hard read/parse error should still surface with defaults available.
		return DefaultPreferences(), err
	}
	return mergeStored(stored), nil
}

// Save atomically writes p to the preferences file, creating the config dir if
// missing (0755). The file is written to a temp file in the same dir and then
// renamed into place (0644).
func Save(baseDir string, p Preferences) error {
	dir, err := configDir(baseDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "preferences-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	dst := filepath.Join(dir, "preferences.json")
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// TemplateKeys returns the sorted list of recognized template keys.
func TemplateKeys() []string {
	keys := make([]string, 0, len(defaultTemplates))
	for k := range defaultTemplates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RecognizedTokens returns the sorted list of tokens Interpolate recognizes.
func RecognizedTokens() []string {
	toks := make([]string, 0, len(recognizedTokens))
	for t := range recognizedTokens {
		toks = append(toks, t)
	}
	sort.Strings(toks)
	return toks
}
