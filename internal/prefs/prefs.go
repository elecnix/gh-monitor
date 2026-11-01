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
	"strings"
	"time"
)

// EventLogConfig turns on the backend event log (issue #86): every update a
// watch consumes is appended to daily JSONL files. Dir empty selects the
// default location (the user cache dir's gh-monitor/events); KeepDays zero
// selects the default retention (10 days). A *EventLogConfig that is nil
// means off — the pointer is the switch.
type EventLogConfig struct {
	Dir      string `json:"dir,omitempty"`
	KeepDays int    `json:"keepDays,omitempty"`
}

// DefaultEventLogKeepDays is the retention window when keepDays is unset.
const DefaultEventLogKeepDays = 10

// Preferences holds the user's notification templates keyed by event kind,
// plus non-template configuration.
//
// Templates is keyed by the event kinds in DefaultPreferences. The non-template
// fields (IgnoredBots, RetriggerComments, SelfUpdate) are plain config and are
// exempt from the template validation guardrail.
//
// SelfUpdate is a global-only setting (issue #82): it decides whether the
// resident daemon upgrades gh-monitor itself, which is a machine-wide act —
// this document is the only place it is read from.
//
// PollInterval and IdlePollCeiling are global-only settings too (issue #90):
// they set the daemon poller's base cadence and idle-backoff ceiling without a
// restart-forgetting-flag dance. Both use the same Go-duration grammar as
// SelfUpdate; "" keeps whatever the --interval flag or built-in default says.
// PollWhenBrokerHealthy (default true) is the third lever: false suspends
// timer-driven fetching entirely while the broker wake path reports healthy,
// so scheduled API spend exists only as insurance against event loss.
//
// ReactOnNotify (default true) adds a 👀 reaction to every comment a watch
// delivers a notification about (review threads, general PR comments, issue
// comments), so humans on the PR see the notification was received. It is
// client-side evidence of delivery, not of action.
//
// AnnotationChecks (default empty) is a per-check-name allowlist for
// check-run annotations: only annotations from the named checks are reported
// (issue #110). An empty list means every check, so the default reports
// everything. It is the same shape as IgnoredBots, and it is what makes the
// check-annotations event usable as a wake trigger: a PR that carries the
// same deprecation warning from six workflow jobs no longer wakes a consumer
// that only cares about one check's findings.
type Preferences struct {
	Templates   map[string]string `json:"templates"`
	IgnoredBots []string          `json:"ignoredBots"`
	// AnnotationChecks names the checks whose annotations are reported.
	// Empty means every check.
	AnnotationChecks      []string `json:"annotationChecks"`
	RetriggerComments     bool     `json:"retriggerComments"`
	SelfUpdate            string   `json:"selfUpdate,omitempty"`
	PollInterval          string   `json:"pollInterval,omitempty"`
	IdlePollCeiling       string   `json:"idlePollCeiling,omitempty"`
	PollWhenBrokerHealthy bool     `json:"pollWhenBrokerHealthy"`
	// CoveredSafetyInterval is how long a repository a sub-daemon covers may
	// go without an event before the daemon fetches an active watch on it
	// once. Unset means DefaultCoveredSafetyInterval; "0" or "false" turns
	// the check off.
	CoveredSafetyInterval string          `json:"coveredSafetyInterval,omitempty"`
	ReactOnNotify         bool            `json:"reactOnNotify"`
	EventLog              *EventLogConfig `json:"eventLog,omitempty"`
}

// DefaultCoveredSafetyInterval is the coveredSafetyInterval default.
const DefaultCoveredSafetyInterval = 30 * time.Minute

// CoveredSafetyInterval interprets a coveredSafetyInterval preference value.
// Unlike the cadence keys, "0" and "false" mean off, because a covered
// repository has no other timer to fall back on; anything unparseable keeps
// the default, so a typo never disables the safety net.
func CoveredSafetyInterval(spec string) time.Duration {
	switch spec {
	case "":
		return DefaultCoveredSafetyInterval
	case "0", "false":
		return 0
	}
	if d, err := time.ParseDuration(spec); err == nil && d > 0 {
		return d
	}
	return DefaultCoveredSafetyInterval
}

// DefaultIdlePollCeiling is the idle-backoff ceiling used when idlePollCeiling
// is unset — the monitor package's own 300s default. Declared here (rather
// than imported) to keep this package decoupled from internal/monitor.
const DefaultIdlePollCeiling = 5 * time.Minute

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
	"new-commit":               "📝 New commit {commitShortOid} pushed to {prLabel}[[ by {commitAuthor}]][[, co-authored by {commitCoauthors}]]. Review the PR description to ensure it still reflects the latest changes.",
	"merged":                   "🔀 PR {prLabel} was merged. Monitoring stopped.",
	"closed":                   "❌ PR {prLabel} was closed. Monitoring stopped.",
	"first-poll":               "📡 Monitoring {prLabel} ({pollMode})",
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

	// API degradation (one per episode: the transition into degraded state,
	// a changed error, and the recovery — not one per failed poll).
	"degraded": "⚠️ API degraded ({degradedSurface}) on {prLabel}: {degradedMessage}",
}

// DefaultPreferences returns a fresh copy of the built-in defaults.
func DefaultPreferences() Preferences {
	templates := make(map[string]string, len(defaultTemplates))
	for k, v := range defaultTemplates {
		templates[k] = v
	}
	return Preferences{
		Templates:             templates,
		IgnoredBots:           []string{},
		AnnotationChecks:      []string{},
		RetriggerComments:     false,
		SelfUpdate:            "",
		PollInterval:          "",
		IdlePollCeiling:       "",
		PollWhenBrokerHealthy: true,
		ReactOnNotify:         true,
	}
}

// SelfUpdateInterval interprets a selfUpdate preference value against dflt,
// the cadence used when self-update is enabled without an explicit duration.
// Zero means off. The spec grammar mirrors what the removed
// GH_MONITOR_SELFUPDATE env variable accepted (issue #82): absent, "0", or
// "false" disable; "1" or "true" select dflt; any positive Go duration is an
// explicit cadence. Anything unparseable is off — never guessed.
func SelfUpdateInterval(spec string, dflt time.Duration) time.Duration {
	switch spec {
	case "", "0", "false":
		return 0
	case "1", "true":
		return dflt
	}
	if d, err := time.ParseDuration(spec); err == nil && d > 0 {
		return d
	}
	return 0
}

// durationOverride interprets a poll-cadence preference value against dflt
// (issue #90). The grammar mirrors what the removed GH_MONITOR_* env variables
// accepted: absent, "0", or "false" keep the caller's default; any positive Go
// duration overrides it; anything else falls back rather than being guessed.
func durationOverride(spec string, dflt time.Duration) time.Duration {
	switch spec {
	case "", "0", "false":
		return dflt
	}
	if d, err := time.ParseDuration(spec); err == nil && d > 0 {
		return d
	}
	return dflt
}

// PollInterval interprets a pollInterval preference value against dflt, the
// interval the daemon's --interval flag (or its default) selected. It accepts
// the same Go-duration grammar as SelfUpdateInterval; "" disables the override
// so the caller falls back to the flag/default.
func PollInterval(spec string, dflt time.Duration) time.Duration {
	return durationOverride(spec, dflt)
}

// IdlePollCeiling interprets an idlePollCeiling preference value against dflt,
// replacing the built-in idle-backoff ceiling for every target — busy or
// quiet, broker-healthy or not. Same grammar as PollInterval.
func IdlePollCeiling(spec string, dflt time.Duration) time.Duration {
	return durationOverride(spec, dflt)
}

// ValidDurationOverride reports whether spec is a value UpdateFile accepts for
// the pollInterval / idlePollCeiling keys, so a typo is rejected at set time
// rather than becoming a silent no-op.
func ValidDurationOverride(spec string) bool {
	switch spec {
	case "", "0", "false":
		return true
	}
	d, err := time.ParseDuration(spec)
	return err == nil && d > 0
}

// ResolveDaemonCadence resolves the three poll-cadence preferences into the
// values the daemon runs with (issue #90). flagInterval is what the --interval
// flag (or its default) selected; the config file overrides it when set.
func ResolveDaemonCadence(p Preferences, flagInterval time.Duration) (interval, idleCeiling time.Duration, pauseWhenHealthy bool) {
	return PollInterval(p.PollInterval, flagInterval),
		IdlePollCeiling(p.IdlePollCeiling, DefaultIdlePollCeiling),
		p.PollWhenBrokerHealthy
}

// ValidSelfUpdateSpec reports whether spec is a value UpdateFile accepts for
// the selfUpdate key. It accepts exactly what SelfUpdateInterval interprets,
// so a typo is rejected at set time rather than becoming a silent no-op.
func ValidSelfUpdateSpec(spec string) bool {
	switch spec {
	case "", "0", "false", "1", "true":
		return true
	}
	d, err := time.ParseDuration(spec)
	return err == nil && d > 0
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
	"pollMode":              true,
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

	"degradedSurface":  true,
	"degradedMessage":  true,
	"degradedSurfaces": true,
}

// tokenRE matches a single {token} placeholder. The token name is captured.
var tokenRE = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// tokenStartRE matches a {token} placeholder at the start of the string it is
// applied to, so a scan can tell "a token starts here" from "a token starts
// further along".
var tokenStartRE = regexp.MustCompile(`^\{([a-zA-Z]+)\}`)

// Interpolate replaces each recognized {token} with vars[token], and drops
// every optional segment whose tokens all render empty.
//
// An optional segment is written [[…]] and holds a clause that is only
// meaningful when it has something to name. The built-in new-commit message
// says "by alice, co-authored by bob" and must not say "co-authored by " when
// the commit carries no Co-authored-by trailer.
//
// A segment is dropped when every {token} inside it resolves to an empty
// value: the token is recognized and either absent from vars or mapped to "".
// A token that is not recognized never resolves to an empty value — it is left
// literal, exactly as outside a segment, so a typo stays visible instead of
// silently vanishing.
//
// The [[…]] markers only mean anything around a recognized {token}. A [[…]]
// holding no recognized token is literal text, markers included, so a
// template that merely mentions brackets ("see [[1]]") keeps reading the way
// its author wrote it.
//
// Segments nest, so a clause that depends on two values can be written
// [[… {a} … [[… {b} …]] …]] and is dropped only when both are empty. A
// segment is kept when any token inside it — including one inside a nested
// segment — resolves to a non-empty value, so a nested clause that still has
// something to say keeps the clause around it alive.
//
// Unrecognized tokens, and recognized tokens absent from vars, are left
// literally in place.
func Interpolate(template string, vars map[string]string) string {
	return renderText(template, vars)
}

// renderText renders a piece of template text: recognized tokens are
// substituted, and every optional segment it holds is resolved.
func renderText(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "[[") {
			body, next, closed := readSegment(s, i+2)
			if !closed {
				// An unclosed "[[" is literal text, and the tokens after it
				// still render. Validate rejects such a template, so it
				// never reaches a consumer.
				b.WriteString("[[")
				i += 2
				continue
			}
			if !holdsRecognizedToken(body) {
				// Not a segment: literal text, markers included.
				b.WriteString(s[i:next])
				i = next
				continue
			}
			if keepSegment(body, vars) {
				b.WriteString(renderText(body, vars))
			}
			i = next
			continue
		}
		if strings.HasPrefix(s[i:], "{") {
			// A token only counts when it starts here: a "{owner}" further
			// along the string is not this position's token.
			if m := tokenStartRE.FindString(s[i:]); m != "" {
				name := m[1 : len(m)-1] // strip { }
				if recognizedTokens[name] {
					if val, ok := vars[name]; ok {
						b.WriteString(val)
						i += len(m)
						continue
					}
				}
				// An unrecognized token, or a recognized one with no value,
				// stays literal.
				b.WriteString(m)
				i += len(m)
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// holdsRecognizedToken reports whether s contains at least one recognized
// {token}. It is what makes "[[…]]" a segment rather than literal text.
func holdsRecognizedToken(s string) bool {
	for _, t := range tokenRE.FindAllStringSubmatch(s, -1) {
		if recognizedTokens[t[1]] {
			return true
		}
	}
	return false
}

// keepSegment reports whether an optional segment's clause is worth keeping.
// A segment whose tokens all resolve to an empty value has nothing to name
// and is dropped. A segment holding an unrecognized token is kept, so a typo
// stays visible.
func keepSegment(body string, vars map[string]string) bool {
	for _, t := range tokenRE.FindAllStringSubmatch(body, -1) {
		name := t[1]
		if !recognizedTokens[name] {
			return true
		}
		if val, ok := vars[name]; ok && val != "" {
			return true
		}
	}
	return false
}

// readSegment reads the body of an optional segment starting at open (the
// index just after the opening "[["). It returns the body, the index just
// after the closing "]]", and whether the segment was closed at all. Nesting
// is tracked, so a "]]" that closes an inner segment does not close this one.
func readSegment(s string, open int) (body string, next int, closed bool) {
	depth := 1
	for i := open; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "[["):
			depth++
			i += 2
		case strings.HasPrefix(s[i:], "]]"):
			depth--
			if depth == 0 {
				return s[open:i], i + 2, true
			}
			i += 2
		default:
			i++
		}
	}
	return s[open:], len(s), false
}

// hasToken reports whether s contains at least one {…} placeholder.
func hasToken(s string) bool {
	return tokenRE.MatchString(s)
}

// hasUnclosedSegment reports whether s opens an optional segment it never
// closes — a "[[" that wraps a recognized {token} with no matching "]]"
// after it. Such a template keeps its markers literally, so it is rejected
// rather than rendered as a broken sentence. Nesting is tracked the way
// readSegment tracks it.
func hasUnclosedSegment(s string) bool {
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], "[[") {
			i++
			continue
		}
		body, next, closed := readSegment(s, i+2)
		if !closed {
			// The rest of the string is the body. It is a segment only if it
			// holds a recognized token.
			return holdsRecognizedToken(body)
		}
		i = next
	}
	return false
}

// Validate rejects unknown template keys, any template value with no {…}
// token (pi's safety guardrail against a template that can never interpolate),
// and any template with an optional segment that is never closed. The
// non-template config (IgnoredBots, RetriggerComments) is exempt.
func Validate(p Preferences) error {
	for key, tmpl := range p.Templates {
		if _, ok := defaultTemplates[key]; !ok {
			return fmt.Errorf("unknown template key: %q", key)
		}
		if !hasToken(tmpl) {
			return fmt.Errorf("template %q has no {token}: %q", key, tmpl)
		}
		if hasUnclosedSegment(tmpl) {
			return fmt.Errorf("template %q has an unclosed [[segment]]: %q", key, tmpl)
		}
	}
	return nil
}

// ConfigPath resolves the preferences file path. When baseDir is non-empty it
// is used as the config base (for tests); otherwise XDG_CONFIG_HOME is used,
// falling back to $HOME/.config.
func ConfigPath(baseDir string) (string, error) {
	dir, err := ConfigDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preferences.json"), nil
}

// ConfigDir returns the gh-monitor config directory — the directory holding
// preferences.json and every other piece of on-disk gh-monitor state (cursor
// stores, the event log). It is the single derivation: callers that need the
// directory itself ask for it here rather than deriving it from ConfigPath
// with their own filepath.Dir / TrimSuffix, which keeps a rename of
// preferences.json from silently splitting state across two directories.
// When baseDir is non-empty it is used as the config base (for tests);
// otherwise XDG_CONFIG_HOME is used, falling back to $HOME/.config.
func ConfigDir(baseDir string) (string, error) {
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
	Templates             map[string]*string `json:"templates"`
	IgnoredBots           []string           `json:"ignoredBots"`
	AnnotationChecks      []string           `json:"annotationChecks"`
	RetriggerComments     *bool              `json:"retriggerComments,omitempty"`
	SelfUpdate            *string            `json:"selfUpdate,omitempty"`
	PollInterval          *string            `json:"pollInterval,omitempty"`
	IdlePollCeiling       *string            `json:"idlePollCeiling,omitempty"`
	PollWhenBrokerHealthy *bool              `json:"pollWhenBrokerHealthy,omitempty"`
	CoveredSafetyInterval *string            `json:"coveredSafetyInterval,omitempty"`
	ReactOnNotify         *bool              `json:"reactOnNotify,omitempty"`
	EventLog              *EventLogConfig    `json:"eventLog,omitempty"`
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
	dir, err := ConfigDir(baseDir)
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
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
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
