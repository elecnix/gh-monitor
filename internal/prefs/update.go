package prefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// UpdateFile applies a JSON override document (matching the stored preferences
// shape) to the preferences file, validates the result, saves it, and returns
// the effective merged Preferences.
//
// The overrides document may contain any subset of the stored shape's keys:
//   - "templates": map of event-kind → string|null. A string sets/overrides
//     that template; null resets it to its default (the key is removed from the
//     file so the built-in default applies). Unknown kinds are rejected.
//   - "ignoredBots": array of strings. Present (even empty) replaces the stored
//     list; null clears it to an empty list. Absent leaves it untouched.
//   - "retriggerComments": bool. Present sets it; null resets it to the default
//     (false) and removes it from the file. Absent leaves it untouched.
//   - "selfUpdate": string (issue #82) — a Go duration ("30m"), "1"/"true"
//     for the daemon's default cadence, or ""/"0"/"false" to disable. Null
//     resets it to the default (off) and removes it from the file. Absent
//     leaves it untouched. Invalid specs are rejected so a typo never becomes
//     a silent no-op.
//   - "pollInterval" / "idlePollCeiling": strings (issue #90) — a positive Go
//     duration ("5m", "6h") overriding the daemon poller's base cadence or
//     idle-backoff ceiling, or ""/"0"/"false" to keep the --interval
//     flag/built-in default. Null resets and removes from the file; invalid
//     specs are rejected at set time.
//   - "pollWhenBrokerHealthy": bool (issue #90, default true). false suspends
//     the daemon's timer-driven fetching while the broker wake path reports
//     healthy; a degrade resumes polling immediately. Null resets to true.
//   - "reactOnNotify": bool (default true). The watch client adds a 👀
//     reaction to every comment a delivered notification is about. false
//     turns it off; null resets it to the default and removes it from the
//     file. Absent leaves it untouched.
//
// Unknown top-level keys are rejected so callers notice typos. An empty object
// ("{}") is a no-op: it returns the current effective preferences without
// writing a file.
//
// The file is always written to the canonical (non-legacy) config path, so a
// set on a legacy-only config migrates it to the new location.
func UpdateFile(baseDir string, overrides []byte) (Preferences, error) {
	stored, err := loadStored(baseDir)
	if err != nil {
		return Preferences{}, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(overrides, &raw); err != nil {
		return Preferences{}, fmt.Errorf("parse overrides: %w", err)
	}

	// Detect whether any override was actually supplied, to support the
	// empty-"{}" no-op (do not create a file for an empty document).
	anyOverride := false

	for key, v := range raw {
		anyOverride = true
		switch key {
		case "templates":
			var t map[string]*string
			if err := json.Unmarshal(v, &t); err != nil {
				return Preferences{}, fmt.Errorf("parse templates: %w", err)
			}
			if stored.Templates == nil {
				stored.Templates = map[string]*string{}
			}
			for k, val := range t {
				if _, ok := defaultTemplates[k]; !ok {
					return Preferences{}, fmt.Errorf("unknown template key: %q", k)
				}
				if val == nil {
					delete(stored.Templates, k)
					continue
				}
				if !hasToken(*val) {
					return Preferences{}, fmt.Errorf("template %q has no {token}: %q", k, *val)
				}
				stored.Templates[k] = val
			}
		case "ignoredBots":
			if string(v) == "null" {
				stored.IgnoredBots = []string{}
			} else {
				var bots []string
				if err := json.Unmarshal(v, &bots); err != nil {
					return Preferences{}, fmt.Errorf("parse ignoredBots: %w", err)
				}
				stored.IgnoredBots = bots
			}
		case "retriggerComments":
			if string(v) == "null" {
				stored.RetriggerComments = nil
			} else {
				var b bool
				if err := json.Unmarshal(v, &b); err != nil {
					return Preferences{}, fmt.Errorf("parse retriggerComments: %w", err)
				}
				stored.RetriggerComments = &b
			}
		case "selfUpdate":
			if string(v) == "null" {
				stored.SelfUpdate = nil
			} else {
				var s string
				if err := json.Unmarshal(v, &s); err != nil {
					return Preferences{}, fmt.Errorf(
						"parse selfUpdate: %w (use a Go duration like \"30m\", \"1\"/\"true\" for the default cadence, or \"\"/\"false\"/\"0\" to disable)", err)
				}
				if !ValidSelfUpdateSpec(s) {
					return Preferences{}, fmt.Errorf(
						"invalid selfUpdate value %q: use a Go duration (\"30m\"), \"1\"/\"true\" (default cadence), or \"\"/\"0\"/\"false\" (off)", s)
				}
				stored.SelfUpdate = &s
			}
		case "pollInterval", "idlePollCeiling":
			if string(v) == "null" {
				switch key {
				case "pollInterval":
					stored.PollInterval = nil
				case "idlePollCeiling":
					stored.IdlePollCeiling = nil
				}
			} else {
				var s string
				if err := json.Unmarshal(v, &s); err != nil {
					return Preferences{}, fmt.Errorf(
						"parse %s: %w (use a Go duration like \"5m\" or \"6h\", or \"\"/\"false\"/\"0\" to keep the flag/default)", key, err)
				}
				if !ValidDurationOverride(s) {
					return Preferences{}, fmt.Errorf(
						"invalid %s value %q: use a positive Go duration (\"5m\") or \"\"/\"0\"/\"false\" (keep the default)", key, s)
				}
				switch key {
				case "pollInterval":
					stored.PollInterval = &s
				case "idlePollCeiling":
					stored.IdlePollCeiling = &s
				}
			}
		case "pollWhenBrokerHealthy":
			if string(v) == "null" {
				stored.PollWhenBrokerHealthy = nil
			} else {
				var b bool
				if err := json.Unmarshal(v, &b); err != nil {
					return Preferences{}, fmt.Errorf(
						"parse pollWhenBrokerHealthy: %w (use true or false; null resets to the default)", err)
				}
				stored.PollWhenBrokerHealthy = &b
			}
		case "reactOnNotify":
			if string(v) == "null" {
				stored.ReactOnNotify = nil
			} else {
				var b bool
				if err := json.Unmarshal(v, &b); err != nil {
					return Preferences{}, fmt.Errorf(
						"parse reactOnNotify: %w (use true or false; null resets to the default)", err)
				}
				stored.ReactOnNotify = &b
			}
		case "eventLog":
			if string(v) == "null" {
				stored.EventLog = nil
			} else {
				var cfg EventLogConfig
				if err := json.Unmarshal(v, &cfg); err != nil {
					return Preferences{}, fmt.Errorf(
						"parse eventLog: %w (use an object like {\"dir\": \"/path\", \"keepDays\": 10}; both fields optional)", err)
				}
				if cfg.KeepDays < 0 {
					return Preferences{}, fmt.Errorf("invalid eventLog keepDays %d: must be zero (default) or positive", cfg.KeepDays)
				}
				stored.EventLog = &cfg
			}
		default:
			return Preferences{}, fmt.Errorf("unknown preference key: %q (valid: templates, ignoredBots, retriggerComments, selfUpdate, pollInterval, idlePollCeiling, pollWhenBrokerHealthy, reactOnNotify, eventLog)", key)
		}
	}

	effective := mergeStored(stored)
	if err := Validate(effective); err != nil {
		return Preferences{}, err
	}

	if !anyOverride {
		return effective, nil
	}
	if err := saveStored(baseDir, stored); err != nil {
		return Preferences{}, err
	}
	return effective, nil
}

// ResetFile removes the preferences file (and any legacy file), restoring all
// defaults. It returns DefaultPreferences and is a no-op if no file exists.
func ResetFile(baseDir string) (Preferences, error) {
	p, err := ConfigPath(baseDir)
	if err != nil {
		return Preferences{}, err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return Preferences{}, fmt.Errorf("remove preferences: %w", err)
	}
	// Best-effort: also clear a legacy file so a reset is complete.
	if legacy, err := legacyPath(baseDir); err == nil {
		_ = os.Remove(legacy)
	}
	return DefaultPreferences(), nil
}

// FilePath returns the canonical preferences file path under the config dir.
func FilePath(baseDir string) (string, error) {
	return ConfigPath(baseDir)
}

// ---------------------------------------------------------------------------
// Internal stored-shape helpers (shared by Load and UpdateFile)
// ---------------------------------------------------------------------------

// loadStored reads the raw stored preferences file (new path, falling back to
// the legacy path). Missing files yield an empty stored shape.
func loadStored(baseDir string) (storedPreferences, error) {
	stored := storedPreferences{Templates: map[string]*string{}}
	path, err := resolvePath(baseDir)
	if err != nil {
		return stored, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return stored, nil
		}
		return stored, fmt.Errorf("read preferences: %w", err)
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return stored, fmt.Errorf("parse preferences %s: %w", path, err)
	}
	if stored.Templates == nil {
		stored.Templates = map[string]*string{}
	}
	return stored, nil
}

// mergeStored overlays a stored shape onto the built-in defaults, returning the
// effective Preferences. Mirrors the overlay rules documented on Load.
func mergeStored(stored storedPreferences) Preferences {
	prefs := DefaultPreferences()
	for key, val := range stored.Templates {
		if _, ok := defaultTemplates[key]; !ok {
			continue
		}
		if val == nil {
			continue
		}
		if !hasToken(*val) {
			continue
		}
		prefs.Templates[key] = *val
	}
	if stored.IgnoredBots != nil {
		prefs.IgnoredBots = stored.IgnoredBots
	}
	if stored.RetriggerComments != nil {
		prefs.RetriggerComments = *stored.RetriggerComments
	}
	if stored.SelfUpdate != nil {
		prefs.SelfUpdate = *stored.SelfUpdate
	}
	if stored.PollInterval != nil {
		prefs.PollInterval = *stored.PollInterval
	}
	if stored.IdlePollCeiling != nil {
		prefs.IdlePollCeiling = *stored.IdlePollCeiling
	}
	if stored.PollWhenBrokerHealthy != nil {
		prefs.PollWhenBrokerHealthy = *stored.PollWhenBrokerHealthy
	}
	if stored.ReactOnNotify != nil {
		prefs.ReactOnNotify = *stored.ReactOnNotify
	}
	if stored.EventLog != nil {
		prefs.EventLog = stored.EventLog
	}
	return prefs
}

// saveStored writes the stored shape to the canonical config path (never the
// legacy path), creating the config dir if missing. The write is atomic.
func saveStored(baseDir string, stored storedPreferences) error {
	dir, err := configDir(baseDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "preferences-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
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
