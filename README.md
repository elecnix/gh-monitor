# gh-monitor

A GitHub CLI extension for inline PR review comments, thread inspection, and live PR monitoring in the terminal — built to be driven by coding agents such as Claude Code.

This fork of [agynio/gh-pr-review](https://github.com/agynio/gh-pr-review) adds features for developers, DevOps teams, and AI systems that need complete pull request review context.

## Contributors

This repository incorporates contributions from the upstream project's pull requests, which appeared to be unmaintained. The following contributors authored the original work that has been integrated: [@casey-brooks](https://github.com/casey-brooks) [@rowan-stein](https://github.com/rowan-stein) [@Benkovichnikita](https://github.com/Benkovichnikita) [@highb](https://github.com/highb) [@EurFelux](https://github.com/EurFelux) [@rileychh](https://github.com/rileychh) [@player3](https://github.com/player3) [@squirrel289](https://github.com/squirrel289)

Pull requests are welcome.

**Blog post:** [gh-pr-review: LLM-friendly PR review workflows in your CLI](https://agyn.io/blog/gh-pr-review-cli-agent-workflows)

## Features

GitHub's built-in `gh` tool does not show inline comments, review threads, or thread grouping. This extension adds:

- View inline review threads with file context
- Reply to comments from the terminal
- Resolve threads programmatically
- Group and inspect threads with `threads view`
- Export structured JSON for LLMs and automation
- Manage pull request draft status (mark as draft/ready for review)
- List all draft pull requests in a repository
- Continuously monitor a PR and stream one event per change (`monitor`) — designed to be wrapped by [Claude Code](https://claude.com/claude-code)'s persistent `Monitor` tool for live, agent-driven PR notifications

## Installation

```sh
gh extension install elecnix/gh-monitor
gh extension upgrade elecnix/gh-monitor  # Update existing installation
```

### Agent Skill

Register with your AI agent using the [SKILL.md](skills/gh-monitor/SKILL.md) definition:

```bash
npx skills add elecnix/gh-monitor
```

## Commands

| Command                         | Description                                                                |
| ------------------------------- | -------------------------------------------------------------------------- |
| _(default)_                     | Continuously watch a PR, streaming one event per change (NDJSON)           |
| `monitor` / `watch`             | Continuously watch a PR, streaming one event per change (NDJSON)           |
| `monitor --run-id <id>`         | Watch a single GitHub Actions workflow run until it completes (NDJSON)     |
| `monitor -R owner/repo`         | Watch a repository for new PRs and issues (NDJSON)                         |
| `draft status`                  | Check if a pull request is a draft                                         |
| `draft mark`                    | Mark a pull request as draft                                               |
| `draft ready`                   | Mark a pull request as ready for review                                    |
| `draft list`                    | List all draft pull requests in the repository                             |
| `review --start`                | Opens a pending review                                                     |
| `review --add-comment`          | Adds inline comment (requires `PRR_…` review node ID)                      |
| `review --edit-comment`         | Updates a comment in a pending review                                      |
| `review --delete-comment`       | Deletes a comment from a pending review                                    |
| `review view`                   | Aggregates reviews, inline comments, and replies                           |
| `review --submit`               | Finalizes a pending review                                                 |
| `comments reply`                | Replies to a review thread                                                 |
| `react`                         | Adds a reaction to any GitHub node (comments, reviews, etc.)               |
| `threads list`                  | Lists review threads for the pull request                                  |
| `threads view`                  | View full conversation for specific threads by ID                          |
| `threads resolve` / `unresolve` | Resolves or unresolves review threads                                      |
| `prefs`                         | View and edit notification preference templates (get/set/reset/path)       |
| `daemon`                        | Run a shared-poller daemon so multiple `monitor` processes share one fetch |

### Filters

| Flag                        | Purpose                                                                                      |
| --------------------------- | -------------------------------------------------------------------------------------------- |
| `--reviewer <login>`        | Only include reviews by specified user (case-insensitive)                                    |
| `--states <list>`           | Comma-separated states: `APPROVED`, `CHANGES_REQUESTED`, `COMMENTED`, `DISMISSED`, `PENDING` |
| `--unresolved`              | Keep only unresolved threads                                                                 |
| `--not_outdated`            | Exclude threads marked as outdated                                                           |
| `--tail <n>`                | Retain only last `n` replies per thread (0 = all)                                            |
| `--include-comment-node-id` | Add comment node identifiers to parent comments and replies                                  |
| `--author <login>`          | Filter threads to those containing a comment by this author login (case-insensitive)         |
| `--include-resolved`        | Include resolved threads (overrides --unresolved)                                            |
| `--mine`                    | Show only threads involving or resolvable by the viewer (threads list only)                  |

**Note**: Commands accepting `--body` also support `--body-file <path>` to read from a file. Use `--body-file -` to read from stdin. These flags are mutually exclusive.

See [skills/references/USAGE.md](skills/references/USAGE.md) for detailed usage. See [docs/SCHEMAS.md](docs/SCHEMAS.md) for JSON response schemas.

## Usage

Basic workflow:

1. Start a review: `gh monitor review --start`
2. Add comments: `gh monitor review --add-comment --review-id <ID> --path <file> --line <N> --body "<msg>"`
3. Submit review: `gh monitor review --submit --review-id <ID> --event APPROVE`
4. Resolve threads: `gh monitor threads resolve --thread-id <ID>`

### Adding Reactions

Add reactions to any GitHub node (comments, reviews, etc.):

```sh
gh monitor react <comment_id> --type thumbs_up
```

Valid reaction types: `thumbs_up`, `thumbs_down`, `laugh`, `hooray`, `confused`, `heart`, `rocket`, `eyes`

When inside a git repository, `-R owner/repo` and PR number are inferred automatically.

### Viewing Reviews

`gh monitor review view` shows all reviews, inline comments, and replies:

```sh
gh monitor review view -R owner/repo --pr 3
```

Common filters:

- `--unresolved` — Show only unresolved threads
- `--reviewer <user>` — Filter by reviewer
- `--states APPROVED,CHANGES_REQUESTED` — Filter by review state

Reply to threads using the `thread_id` from the view output:

```sh
gh monitor comments reply --thread-id <ID> --body "<msg>"
```

### Managing Threads

List and resolve threads:

```sh
# List unresolved threads
gh monitor threads list --unresolved

# List only your threads
gh monitor threads list --mine

# Resolve a thread
gh monitor threads resolve --thread-id <ID>

# View full conversation for specific threads
gh monitor threads view <thread_id> <thread_id>
```

### Managing Draft Status

Check and manage pull request draft status:

```sh
# Check if PR is a draft
gh monitor draft status --repo owner/repo --pr 123

# Mark PR as draft
gh monitor draft mark --repo owner/repo --pr 123

# Mark PR as ready for review
gh monitor draft ready --repo owner/repo --pr 123

# List all draft PRs in repository
gh monitor draft list --repo owner/repo
```

### Deleting Comments

Delete a comment from a pending review:

```sh
gh monitor review --delete-comment --comment-id <comment_id>
```

This only works on comments in pending reviews. Once a review is submitted, comments cannot be deleted.

### Monitoring a PR (streaming)

The default command — invoked as `gh monitor <selector> [flags]` without a subcommand — watches a PR continuously. The `monitor` and `watch` subcommands are also available as explicit forms.

`monitor` runs continuously and emits **one event per genuinely-new change** — new review threads, general comments, failing/green CI, merge conflicts, review decisions, new commits, and merge/close. Each event is one NDJSON line on stdout, so a persistent watcher can surface each line as it arrives. The loop auto-stops when the PR is merged or closed, and idle polling backs off exponentially (capped at 5 minutes).

```sh
# Default: stream events until the PR is merged/closed (NDJSON, one event per line)
gh monitor -R owner/repo 42

# Human-readable rendered messages instead of JSON
gh monitor --text -R owner/repo 42

# One-shot: emit the current actionable state and exit
gh monitor --once -R owner/repo 42
```

**Monitor flags:**

- `--interval <seconds>` - Base polling interval (default: 60, min 10)
- `--timeout <seconds>` - Maximum watch time (default: 0 = until merged/closed)
- `--ignored-bots <a,b>` - Author logins whose general comments are ignored
- `--once` - Fetch once, emit the current actionable state, and exit
- `--text` - Emit the rendered message per event instead of NDJSON

`new-unresolved-threads` and `new-general-comments` events carry a rich `detail` body — the thread/comment location, author, text, a diff excerpt centered on the anchored line, and the exact commands to reply/resolve or 👍-acknowledge — so a consumer can act without extra API calls. In `--text` mode the PR label and commit SHA are wrapped in OSC-8 hyperlinks (clickable in supporting terminals, plain text elsewhere) and any `detail` body is printed, indented, beneath the message.

Set `retriggerComments: true` in the preferences file to re-emit every open unresolved thread and general comment on _each_ poll (instead of only genuinely-new ones). This is chatty and effectively disables the idle backoff, so pair it with a longer `--interval`. Check/CI/review/commit/state events still de-duplicate normally.

Notification wording is templated and user-overridable via `${XDG_CONFIG_HOME:-~/.config}/gh-monitor/preferences.json`. Use the [`prefs`](#managing-preferences) command to view and edit it without touching the file by hand.

#### Use with Claude Code

`monitor` is designed to be wrapped by [Claude Code](https://claude.com/claude-code)'s persistent `Monitor` tool: each NDJSON line becomes a session notification, so the agent reacts to review comments, CI failures, conflicts, and new commits as they happen. The command handles polling and change-detection; the harness handles delivery and turn-batching (events that arrive mid-turn are queued and flushed when the turn ends).

In practice you don't write the tool call yourself — you ask Claude Code in plain language, e.g.:

> Monitor PR 42 in this repo and address review comments as they come in.

Claude then registers a persistent monitor whose command is this tool:

```
Monitor({
  command: "gh monitor -R owner/repo 42",
  persistent: true,
  description: "PR owner/repo#42 events",
})
```

The watch runs in the background while Claude works, and **auto-stops** when the PR is merged or closed. To stop it earlier, tell Claude to stop monitoring (it calls `TaskStop`). Because 👍-acknowledged comments are dropped from the stream, the loop-breaker is: reply/fix, then resolve the thread or react 👍 — that item won't notify again.

See [docs/CLAUDE_CODE.md](docs/CLAUDE_CODE.md) for the full guide, the event→reaction mapping, template customization, and a hook that auto-suggests monitoring right after `gh pr create`.

### Monitoring a workflow run

Use `--run-id <id>` to watch a **single GitHub Actions workflow run** until it reaches a terminal conclusion. This works for any non-PR run — deploy workflows on `main`, `workflow_dispatch` runs, scheduled runs, etc. — and is the counterpart to PR CI watching: instead of polling a PR for check suites, it polls the run's `status`/`conclusion` and emits one event per genuinely-new transition (`run-queued`, `run-in-progress`, `run-completed`). The loop **auto-stops** when the run's status becomes `completed`.

The run id is the numeric id in a run's URL: `…/actions/runs/<id>` (also `databaseId` from `gh run list`).

```sh
# Watch run 30433642 until it completes (NDJSON, one event per line)
gh monitor --run-id 30433642 -R owner/repo

# One-shot: emit the current state (e.g. a run already finished) and exit
gh monitor --run-id 30433642 -R owner/repo --once

# Human-readable rendered messages instead of JSON
gh monitor --run-id 30433642 -R owner/repo --text
```

The `run-completed` event carries the run's `conclusion` (`success`, `failure`, `timed_out`, `cancelled`, `neutral`, `action_required`, `stale`, `skipped`) as structured JSON, plus `run_id`, the run URL, and the head commit. When the conclusion is a failure variant (`failure`, `timed_out`, `cancelled`, `action_required`), the event also carries a `detail` body with a truncated snippet of the failed-job logs (the first 50 lines of `gh run view <run-id> --log-failed`), so an agent can diagnose the failure without an extra turn. The same `--interval`, `--timeout`, `--once`, `--text`, and `-R` flags apply. `--run-id` is mutually exclusive with the PR selector and `--ref`/`--commit`/`--issue`.

### Monitoring a repository for new PRs and issues

Use `--repo` alone (without a PR number, ref, commit, issue, or run-id) to watch an **entire repository** for new PRs and issues. The loop polls the repository and emits a `repo-new-pr` or `repo-new-issue` event for each genuinely-new item. Unlike PR/issue/run targets, repo targets never auto-stop — they run until cancelled or timed out.

```sh
# Watch a repo for new PRs and issues (NDJSON, one event per line)
gh monitor -R owner/repo

# One-shot: emit the current state and exit
gh monitor -R owner/repo --once

# Human-readable rendered messages instead of JSON
gh monitor -R owner/repo --text

# With a timeout (stops after 1 hour)
gh monitor -R owner/repo --timeout 3600
```

Each event carries the item's number, title, author, and URL in the `repo_items` array. Templates for `repo-new-pr` and `repo-new-issue` are configurable via `prefs`.

### Shared poller daemon

By default every `gh monitor` process polls GitHub on its own `--interval` cadence. When many agents watch the same PR (an orchestrator plus each agent watching the PR it owns), that is N independent fetches for the same data. The `daemon` command runs a long-lived process that maintains **one fetch loop per PR** and fans each fetched snapshot out to every attached `monitor` client, so N processes share a single fetch ([#34](https://github.com/elecnix/gh-monitor/issues/34)).

```sh
# Start the daemon (runs until SIGTERM/SIGINT)
gh monitor daemon --interval 60

# Now any number of `gh monitor` clients watching the same PR share one fetch
gh monitor -R owner/repo 42
gh monitor -R owner/repo 42 --text
```

A `monitor` client detects the daemon via its Unix socket and streams from it instead of polling. When no daemon is running, `monitor` falls back to its usual in-process polling, so existing behaviour is unchanged. Each client keeps its **own baseline** snapshot, so consumption by one client never suppresses delivery to another — the core requirement behind [#32](https://github.com/elecnix/gh-monitor/issues/32).

The socket path honours `$GH_MONITOR_SOCK`, then `$XDG_RUNTIME_DIR/gh-monitor.sock`, then `~/.cache/gh-monitor/daemon.sock`. Set `GH_MONITOR_DAEMON=0` to force a client to use in-process polling even when a daemon is running. `--once` and non-PR targets always use the in-process loop.

### Managing preferences

`gh monitor prefs` views and edits the notification templates and config stored in `~/.config/gh-monitor/preferences.json` (the legacy `~/.config/gh-pr-monitor/preferences.json` is read as a fallback). Editing via `prefs` always writes to the canonical path, so it migrates a legacy config on first use.

```sh
# Print the effective preferences (built-in defaults merged with file overrides)
gh monitor prefs            # or: gh monitor prefs get

# Merge overrides and save (a null template resets that key to its default)
gh monitor prefs set '{"templates":{"conflict":"⚠️ {prLabel} conflict!"}}'
gh monitor prefs set '{"templates":{"merged":null},"ignoredBots":["dependabot"]}'

# Read overrides from a file or stdin
gh monitor prefs set --file overrides.json
echo '{"retriggerComments":true}' | gh monitor prefs set --file -

# Reset everything to the built-in defaults
gh monitor prefs reset

# Show the preferences file path
gh monitor prefs path
```

The document shape is `{ "templates": {"<event-kind>": "<template>" | null}, "ignoredBots": ["login", …], "retriggerComments": false }`. Event kinds and template tokens are listed in `gh monitor prefs --help`. A `--config-dir <dir>` flag overrides the config location (handy for testing).

### Additional Flags

**Review start:**

- `--commit <sha>` - Pin the pending review to a specific commit (defaults to current head)

**Add comment:**

- `--side <LEFT|RIGHT>` - Diff side for inline comment (default: RIGHT)
- `--start-line <n>` - Start line for multi-line comments
- `--start-side <LEFT|RIGHT>` - Start side for multi-line comments

**Comments reply:**

- `--review-id <ID>` - GraphQL review identifier when replying inside a pending review

See [skills/references/USAGE.md](skills/references/USAGE.md) for detailed usage examples.

## Development

Run tests and linters locally with CGO disabled (matching release build):

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 golangci-lint run
```

Releases use the [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) workflow to publish binaries for macOS, Linux, and Windows.

Release descriptions are updated by an AI agent using a template and git commands to generate commit-based changelogs.

Release note template:

```markdown
## What's Changed

### New Features

- feat: <description> ([commit_hash](<[%3Chash%3E](%3Ccommit_url%3E)>))

### Bug Fixes

- fix: <description> ([commit_hash](<[%3Chash%3E](%3Ccommit_url%3E)>))

### Chores

- chore/docs/test: <description> ([commit_hash](<[%3Chash%3E](%3Ccommit_url%3E)>))
```

Git log command:

```sh
git log <previous_tag>..<current_tag> --pretty=format:"- %s ([%h](<commit_url>))"
```
