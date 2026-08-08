# Using `gh-monitor` with Claude Code

`gh monitor` is built to be driven by [Claude Code](https://claude.com/claude-code)'s **persistent `Monitor`** tool. The command runs a poll loop and prints **one NDJSON line per genuinely-new change** on a pull request; Claude Code's `Monitor` streams each line back into the session as a notification, so the agent reacts to review comments, CI failures, conflicts, and new commits as they happen — without polling and without burning tokens between events.

This mirrors the behavior of the `pi-ghpr-monitor` extension for the `pi` agent, but instead of the tool injecting notifications itself, the CLI does the polling + change-detection and the Claude Code harness does the delivery.

## The recipe

Ask Claude Code to monitor a PR, and it registers a persistent monitor whose command is this tool:

```
Monitor({
  command: "gh monitor -R owner/repo 42",
  persistent: true,
  description: "PR owner/repo#42 events",
})
```

- `persistent: true` keeps the watch alive for the whole session (no timeout) — right for PR monitoring.
- Each stdout line (one NDJSON event) becomes a session notification, delivered even while Claude is idle. If a line arrives mid-turn, the harness queues it and flushes when the turn ends — the same "don't interrupt an active turn" behavior `pi-ghpr-monitor` implemented by hand.
- The command **auto-stops** (exits 0) when the PR is merged or closed, ending the watch. To stop earlier, `TaskStop` the monitor.

You generally do not type the `Monitor(...)` call yourself — you say something like _"monitor PR 42 in this repo and address review comments as they come in,"_ and Claude sets up the watch.

## Why the loop is safe to leave running

- **No token cost between events.** The poll and change-detection run inside the CLI process; Claude's context is only touched when a real event is emitted.
- **No duplicate notifications.** The loop diffs each poll against the previous snapshot and only emits genuinely-new changes. 👍-acknowledged comments and threads are dropped, so re-reacting to the same bot comment doesn't re-fire.
- **Adaptive backoff.** After 3 no-change polls the interval grows exponentially (capped at 5 minutes) and resets on any change, so an idle PR is cheap to watch.
- **Resilient.** Transient GitHub API errors are logged to stderr and retried with a separate backoff; the loop does not crash.

## Reacting to events

Each NDJSON line has a stable `type` and a rendered `message`, plus event-relevant fields. See [SCHEMAS.md](SCHEMAS.md) for the full field list. Typical agent reactions:

| `type`                     | Suggested reaction                                                                    |
| -------------------------- | ------------------------------------------------------------------------------------- |
| `new-unresolved-threads`   | Reply to and resolve each thread (`gh monitor comments reply` / `threads resolve`)    |
| `new-general-comments`     | Address the comment; 👍-react to acknowledge non-actionable ones (`gh monitor react`) |
| `new-failing-checks`       | Inspect the failing checks and push a fix                                             |
| `conflict`                 | Rebase / resolve the merge conflict                                                   |
| `review-changes-requested` | Address the requested changes                                                         |
| `new-commit`               | Re-check the PR description still reflects the changes                                |
| `merged` / `closed`        | Monitoring has stopped — wrap up                                                      |

### Monitoring a workflow run

The same `Monitor` tool can watch a single non-PR GitHub Actions run (deploys on `main`, `workflow_dispatch`, scheduled runs) until it completes. The command auto-stops when the run's `status` becomes `completed`.

```
Monitor({ command: "gh monitor --run-id 30433642 -R owner/repo", persistent: true })
```

| `type`            | Suggested reaction                                                                                                                     |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `run-in-progress` | Run started — note it; nothing to do yet                                                                                               |
| `run-completed`   | Inspect the `conclusion` field. On `failure`/`timed_out`, investigate the run logs and fix; on `success`, proceed with dependent steps |

Because 👍 acknowledgment silences a comment, the loop-breaker is: **fix or reply, then react 👍 (or resolve the thread)**, and that item won't notify again.

## Customizing the notification wording

Notification text is templated and user-overridable. Edit:

```
${XDG_CONFIG_HOME:-~/.config}/gh-monitor/preferences.json
```

Each template is a string with `{token}` placeholders (e.g. `{prLabel}`, `{failingChecks}`, `{commitAuthor}`). A template value with no `{token}` is rejected; set a key to `null` to reset it to the built-in default. Non-template config keys `ignoredBots` (author logins to silence) and `retriggerComments` live in the same file. See the `monitor` section of the [README](../README.md) for the token and key list.

## Auto-start on `gh pr create`

To be nudged to start a monitor the moment you open a PR, add a Claude Code
[`PostToolUse` hook](https://docs.claude.com/en/docs/claude-code/hooks) that
watches `Bash` invocations, and when one is a `gh pr create`, extracts the
created PR URL from the command output and prints a reminder. The hook's stdout
becomes context Claude sees, so it reads the suggestion and can register the
`Monitor` for you. This mirrors `pi-ghpr-monitor`'s `prCreateNudge`.

In `~/.claude/settings.json` (or a project `.claude/settings.json`):

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "jq -r 'select(.tool_name==\"Bash\" and (.tool_input.command | test(\"gh pr create\"))) | .tool_response.stdout // \"\"' | grep -oE 'https://github\\.com/[^ ]+/pull/[0-9]+' | head -n1 | sed 's|^|Start monitoring: gh monitor |'"
          }
        ]
      }
    ]
  }
}
```

The one-liner reads the hook's JSON payload on stdin: it fires only for a `Bash`
call whose command contains `gh pr create`, pulls the first `.../pull/<n>` URL
out of the command's stdout, and emits e.g.
`Start monitoring: gh monitor https://github.com/owner/repo/pull/42`.
When the command wasn't a `gh pr create` (or created no PR) the pipeline prints
nothing and the hook is a no-op.

## Auto-start on `gh issue create`

To be nudged to start a monitor the moment you open an issue, add a Claude Code
[`PostToolUse` hook](https://docs.claude.com/en/docs/claude-code/hooks) that
watches `Bash` invocations, and when one is a `gh issue create`, extracts the
created issue URL from the command output and prints a reminder.

Add this hook alongside the `gh pr create` hook in `~/.claude/settings.json`
(or a project `.claude/settings.json`):

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "jq -r 'select(.tool_name==\"Bash\" and (.tool_input.command | test(\"gh issue create\"))) | .tool_response.stdout // \"\"' | grep -oE 'https://github\\.com/[^ ]+/issues/[0-9]+' | head -n1 | sed 's|^|Monitor this issue: gh monitor |'"
          }
        ]
      }
    ]
  }
}
```

The one-liner reads the hook's JSON payload on stdin: it fires only for a `Bash`
call whose command contains `gh issue create`, pulls the first `.../issues/<n>`
URL out of the command's stdout, and emits e.g.
`Monitor this issue: gh monitor https://github.com/owner/repo/issues/42`.
When the command wasn't a `gh issue create` (or created no issue) the pipeline
prints nothing and the hook is a no-op.

## Relationship to `await`

The `await` command has been removed. Use these `monitor` equivalents:

| Old `await` command            | New `monitor` equivalent | Notes                                                                                                                  |
| ------------------------------ | ------------------------ | ---------------------------------------------------------------------------------------------------------------------- |
| `await --check-only`           | `monitor --once`         | Emits one or more NDJSON notifications instead of a single JSON `AwaitResult`; use `--text` for human-readable output. |
| `await --mode all --timeout N` | `monitor --timeout N`    | Continuous streaming instead of a single result at timeout.                                                            |
| `await --mode comments`        | not directly replaced    | Use `monitor` — the event types (`new-unresolved-threads`, `new-general-comments`) give finer-grained control.         |
