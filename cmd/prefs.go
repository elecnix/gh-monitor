package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/prefs"
)

// newPrefsCommand builds the `prefs` command tree for viewing and editing
// notification preferences. The bare `gh monitor prefs` (no subcommand) is an
// alias for `prefs get`.
func newPrefsCommand() *cobra.Command {
	opts := &prefsOptions{}

	cmd := &cobra.Command{
		Use:   "prefs",
		Short: "View and edit notification preferences",
		Long: `View and edit notification preference templates.

Preferences are stored as JSON at the path shown by 'gh monitor prefs path'
(~/.config/gh-monitor/preferences.json by default; the legacy
~/.config/gh-pr-monitor/preferences.json is read as a fallback).

The document shape:
  {
    "templates":            { "<event-kind>": "<template>" | null, ... },
    "ignoredBots":          ["login", ...],
    "retriggerComments":    false,
    "selfUpdate":           "30m" | "1" | "" | null,
    "pollInterval":         "10m" | "" | null,
    "idlePollCeiling":      "6h" | "" | null,
    "pollWhenBrokerHealthy": true | false | null,
    "reactOnNotify":        true | false | null,
    "eventLog":             { "dir": "/path", "keepDays": 10 } | null
  }

selfUpdate is a global-only setting (issue #82): a Go duration for the
resident daemon's release-check cadence, "1"/"true" for the default (hourly),
or ""/"0"/"false"/null to disable (the default).

The poll-cadence keys are global-only settings too (issue #90), read by the
daemon at start:

  pollInterval sets the poller's base cadence, overriding --interval. A Go
duration ("10m"), or ""/"0"/"false"/null to keep the flag/default.

  idlePollCeiling caps the exponential idle backoff for every target — busy
or quiet, broker-healthy or not — replacing the built-in 300s ceiling. A Go
duration ("6h"), or ""/"0"/"false"/null for the default.

  pollWhenBrokerHealthy (default true): false suspends timer-driven fetching
entirely while the broker wake path reports healthy; a degrade resumes
polling immediately. Scheduled API spend becomes pure insurance against event
loss.

reactOnNotify (default true): every comment a delivered notification is about
gets a 👀 reaction — review threads (first comment), general PR comments, and
issue comments — so humans on the PR can see the notification was received.
It is evidence of delivery, not of action. false turns it off; null resets to
the default.

After changing these (or selfUpdate), run 'gh monitor reload' to apply them
to the resident daemon immediately — the restart is state-preserving.

eventLog turns on the backend event log (issue #86): every update a watch
consumes — from any backend — is appended to daily JSONL files. Both fields
are optional: dir defaults to the user cache dir's gh-monitor/events, and
keepDays defaults to 10 (a new file per day, 10 days kept). null or absence
disables it.

A null template value resets that key to its built-in default. Event kinds
include: new-unresolved-threads, new-general-comments, conflict,
new-failing-checks, ci-all-green, review-approved, review-changes-requested,
review-dismissed, new-commit, merged, closed, first-poll, all-clear,
issue-closed, issue-reopened, issue-new-comment, issue-mention, run-queued,
run-in-progress, run-completed.

Template tokens: {owner} {repo} {number} {host} {prLabel} {prUrl}
{unresolvedThreads} {generalComments} {failingChecks} {conflict} {intervalSec}
{reviewAuthor} {commitOid} {commitShortOid} {commitUrl} {commitAuthor}
{commitCoauthors} {commitMessageHeadline} {issueState} {issueTitle}
{issueComments} {runId} {runName} {runNumber} {runEvent} {runStatus}
{runConclusion} {runBranch} {runUrl}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrefsGet(cmd, opts)
		},
	}

	cmd.PersistentFlags().StringVar(&opts.ConfigDir, "config-dir", "", "Directory holding preferences.json (defaults to XDG_CONFIG_HOME/gh-monitor)")

	cmd.AddCommand(newPrefsGetCommand(opts))
	cmd.AddCommand(newPrefsSetCommand(opts))
	cmd.AddCommand(newPrefsResetCommand(opts))
	cmd.AddCommand(newPrefsPathCommand(opts))

	return cmd
}

type prefsOptions struct {
	ConfigDir string
}

func newPrefsGetCommand(opts *prefsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print the effective preferences as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrefsGet(cmd, opts)
		},
	}
}

func runPrefsGet(cmd *cobra.Command, opts *prefsOptions) error {
	p, err := prefs.Load(opts.ConfigDir)
	if err != nil {
		return err
	}
	return encodeJSON(cmd, p)
}

// daemonReadPrefKeys are the settings only the resident daemon reads: they
// take effect when a daemon starts, not when the file is written. Listed in
// one place so the prefs-set hint and any future reader stay in sync.
var daemonReadPrefKeys = []string{"selfUpdate", "pollInterval", "idlePollCeiling", "pollWhenBrokerHealthy"}

// warnDaemonRestartHint prints a stderr hint (stdout stays clean JSON for
// callers that pipe it) when an override touches a daemon-read key, naming
// `gh monitor reload` so applying the change is one command away — not
// silently pending until some future restart.
func warnDaemonRestartHint(cmd *cobra.Command, data []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return // UpdateFile already rejected unparseable overrides
	}
	var touched []string
	for _, k := range daemonReadPrefKeys {
		if _, ok := raw[k]; ok {
			touched = append(touched, k)
		}
	}
	if len(touched) == 0 {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"ℹ️  %s: read by the resident daemon at start — run `gh monitor reload` to apply now (watching state carries over)\n",
		strings.Join(touched, ", "))
}

func newPrefsSetCommand(opts *prefsOptions) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "set [<json>]",
		Short: "Merge preference overrides (JSON) into the file and print the result",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var data []byte
			switch {
			case file == "-":
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				data = b
			case file != "":
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("read --file: %w", err)
				}
				data = b
			case len(args) == 1:
				data = []byte(args[0])
			default:
				return errors.New("provide a JSON argument or use --file (use '-' for stdin)")
			}
			eff, err := prefs.UpdateFile(opts.ConfigDir, data)
			if err != nil {
				return err
			}
			warnDaemonRestartHint(cmd, data)
			return encodeJSON(cmd, eff)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Read overrides from a file (use '-' for stdin)")
	return cmd
}

func newPrefsResetCommand(opts *prefsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Reset preferences to the built-in defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			eff, err := prefs.ResetFile(opts.ConfigDir)
			if err != nil {
				return err
			}
			return encodeJSON(cmd, eff)
		},
	}
}

func newPrefsPathCommand(opts *prefsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the preferences file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := prefs.FilePath(opts.ConfigDir)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
}
