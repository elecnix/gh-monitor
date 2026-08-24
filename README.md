# gh-monitor

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/elecnix/gh-monitor)

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
- Swap the monitoring backend for your own, as a Go library or a separate process ([docs/BACKENDS.md](docs/BACKENDS.md))

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

| Command                         | Description                                                                          |
| ------------------------------- | ------------------------------------------------------------------------------------ |
| _(default)_                     | Continuously watch a PR, streaming one event per change (NDJSON)                     |
| `monitor` / `watch`             | Continuously watch a PR, streaming one event per change (NDJSON)                     |
| `monitor --run-id <id>`         | Watch a single GitHub Actions workflow run until it completes (NDJSON)               |
| `monitor -R owner/repo`         | Watch a repository for new PRs and issues (NDJSON)                                   |
| `monitor -R owner/repo --once`  | Repo-wide merge-readiness view: which PRs can be merged now, and by whom             |
| `draft status`                  | Check if a pull request is a draft                                                   |
| `draft mark`                    | Mark a pull request as draft                                                         |
| `draft ready`                   | Mark a pull request as ready for review                                              |
| `draft list`                    | List all draft pull requests in the repository                                       |
| `review --start`                | Opens a pending review                                                               |
| `review --add-comment`          | Adds inline comment (requires `PRR_…` review node ID)                                |
| `review --edit-comment`         | Updates a comment in a pending review                                                |
| `review --delete-comment`       | Deletes a comment from a pending review                                              |
| `review view`                   | Aggregates reviews, inline comments, and replies                                     |
| `review --submit`               | Finalizes a pending review                                                           |
| `comments reply`                | Replies to a review thread                                                           |
| `react`                         | Adds a reaction to any GitHub node (comments, reviews, etc.)                         |
| `threads list`                  | Lists review threads for the pull request                                            |
| `threads view`                  | View full conversation for specific threads by ID                                    |
| `threads resolve` / `unresolve` | Resolves or unresolves review threads                                                |
| `prefs`                         | View and edit notification preference templates (get/set/set from file/reset/path)   |
| `daemon`                        | Run a shared-poller daemon so multiple `monitor` processes share one fetch           |
| `reload`                        | Restart the resident daemon so it re-reads preferences (watching state carries over) |
| `instances list`                | List all named instance cursors                                                      |
| `instances reset <name>`        | Reset a cursor so the next run replays from the beginning                            |
| `backends`                      | List the monitoring backends and the target kinds they cover                         |
| `version` / `--version`         | Print the running binary's version, VCS revision, and build time                     |

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

### Backend selection

| Flag                       | Purpose                                                                            |
| -------------------------- | ---------------------------------------------------------------------------------- |
| `--backend <name>`         | Pin monitoring to a named backend                                                  |
| `--backend-endpoint <uri>` | Connect an external backend: `unix:<path>`, `tcp:<host:port>`, or `exec:<command>` |

**Note**: Commands accepting `--body` also support `--body-file <path>` to read from a file. Use `--body-file -` to read from stdin. These flags are mutually exclusive.

See [skills/references/USAGE.md](skills/references/USAGE.md) for detailed usage. See [docs/SCHEMAS.md](docs/SCHEMAS.md) for JSON response schemas. See [docs/BACKENDS.md](docs/BACKENDS.md) for writing a monitoring backend.

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

- `--interval <seconds>` - Base polling interval (default: 300, min 10)
- `--timeout <seconds>` - Maximum watch time (default: 0 = until merged/closed)
- `--ignored-bots <a,b>` - Author logins whose general comments are ignored
- `--events <kind,kind>` (alias `--only-events`) - Allowlist of event kinds to emit; suppresses every other kind. Omit to emit everything (the default). Unknown kinds are rejected so a typo fails loudly instead of silently muting what you wanted.
- `--annotation-levels <level,level>` - Annotation levels to surface: `notice`, `warning`, `failure`, or `none`. Default: `warning,failure`. Case-insensitive; unknown values are rejected. `none` drops all annotation events.
- `--once` - Fetch once, emit the current actionable state, and exit
- `--text` - Emit the rendered message per event instead of NDJSON

#### Reducing notification noise with `--events`

By default `gh monitor` emits a notification for every event kind: every CI transition, every comment, every review, every new commit, plus the merge-blocking ones. An orchestrator or automation caller that only wants to act on a subset can pass `--events` (alias `--only-events`) with a comma-separated allowlist; events whose kind is not in the list are suppressed before they reach stdout.

```sh
# Only act on merge conflicts, newly-failing checks, and the terminal merge/close:
gh monitor -R owner/repo 42 --events conflict,new-failing-checks,merged,closed

# Watch a workflow run but only notify on completion (skip queued/in-progress):
gh monitor --run-id 30433642 -R owner/repo --events run-completed
```

The recognised event kinds are the notification template keys: `new-unresolved-threads`, `new-general-comments`, `conflict`, `new-failing-checks`, `ci-all-green`, `review-approved`, `review-changes-requested`, `review-dismissed`, `new-commit`, `merged`, `closed`, `first-poll`, `all-clear`, `issue-closed`, `issue-reopened`, `issue-new-comment`, `issue-mention`, `run-queued`, `run-in-progress`, `run-completed`, `repo-new-pr`, `repo-new-issue`, `check-annotations`, `degraded`. Matching is case-insensitive. An empty allowlist suppresses everything; omit the flag to emit everything.

`degraded` reports that an API surface (graphql or rest) could not be read. It is emitted per **episode**, not per failed poll, so an outage costs a consumer a handful of notifications instead of one per poll: once when a surface degrades, once more if the error message changes while it stays degraded, and once — a ✅ recovery notice — when the next successful poll shows it is back. Like every other kind it has a preferences template key (rewordable in the preferences file) and can be included in or excluded from `--events`; leaving it out of an allowlist mutes the warnings entirely.

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

### Monitoring a branch (ref watch)

Use `--ref <branch>` to watch a remote branch's head: each poll fetches the ref's current OID, author, headline, and CI state, emitting `new-commit` when someone pushes, plus `new-failing-checks` and `ci-all-green` for that branch's checks. This is the counterpart to PR watching for shared branches — most often "tell me when `main` moves so I know to rebase."

```sh
gh monitor -R owner/repo --ref main
```

The first poll normally establishes the baseline silently: whatever the branch points at when the watch starts is treated as already-seen. That is correct for a standing watch but wrong when the caller last looked at the branch _before_ starting it — a push landing in that gap is silently absorbed into the baseline and never reported. `--baseline <oid>` closes this race: pass the commit OID you actually observed (never re-derive it at watch time), and the first poll diffs against it instead.

```sh
# The agent observes the branch, sees this output, then passes the OID verbatim:
git fetch origin && git rev-parse origin/main
# → 3f9c2ab1def0123456789abcdef0123456789ab
gh monitor -R owner/repo --ref main --baseline 3f9c2ab1def0123456789abcdef0123456789ab
```

The OID is resolved through GitHub at startup, so a short SHA expands to the exact full form the API reports and a typo'd or inaccessible SHA fails loudly instead of silently never matching; check state at observation time is captured too, so only _newly_ failing checks surface. `--baseline` requires `--ref` and cannot be combined with `--instance`, whose stored cursor already provides a baseline.

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

### Monitoring a repository (merge-readiness view)

Use `--repo` alone (without a PR number, ref, commit, issue, or run-id) for a **repo-wide merge-readiness view**: one aggregate line per cycle showing which PRs are ready to merge and what every blocked PR is waiting for. This replaces the previous per-PR event stream — which answered "what appeared?" — with the answer to "what can I merge right now, and what is each blocked PR waiting on?"

```sh
# One-shot: show the merge-readiness snapshot and exit
gh monitor -R owner/repo --once

# Continuous: poll on the configured interval
gh monitor -R owner/repo --interval 60

# Human-readable rendered messages instead of JSON
gh monitor -R owner/repo --once --text

# Classify readiness as a specific viewer
gh monitor -R owner/repo --once --viewer octocat
```

**Output format** — one line per cycle:

```
staging=success open=9 ready=[959 957] not-ready=[958(needs-codeowner) 956(awaiting:review) 828(red:gofmt)] others=[953@dependabot:red 573@octocat:ready]
```

**Buckets:**

- **ready** — authored by the viewer, mergeable, all required checks present and green
- **not-ready** — authored by the viewer, with a reason (`CONFLICTS`, `red:<check>`, `pending:<check>`, `awaiting:<check>`, `needs-codeowner`, `changes-requested`, `no-ci`, `mergeability-unknown`, `truncated`, `ruleset:<error>`)
- **others** — authored by someone else; reason always included. Non-viewer PRs that would be \"ready\" appear as `others` with reason `ready` — the viewer can review and merge them.
- **unknown** — surfaced explicitly rather than silently dropped (indicates a bug)

Every open PR appears in exactly one bucket and the counts reconcile against the open total. A degraded API read produces `staging=degraded` rather than silently omitting or guessing.

**`--viewer <login>`** sets whose perspective to classify from (default: authenticated user, resolved from the `gh` CLI). This determines:

- `BLOCKED` + viewer's own PR → `needs-codeowner` (GitHub never counts self-approval; needs a different code owner)
- `BLOCKED` + someone else's PR → `awaiting:review` (the viewer can review and merge)

**Flags:**

- `--viewer <login>` - GitHub login to classify readiness by (default: authenticated user)
- `--interval <seconds>` - Polling interval (default: 300, min 10)
- `--timeout <seconds>` - Maximum watch time (default: 0 = run forever)
- `--once` - Fetch once, emit the current readiness snapshot, and exit
- `--text` - Emit the formatted line instead of NDJSON

#### Named instances with resumable cursors

`--instance <name>` enables a per-instance cursor so a restart resumes from where it left off. For **repo targets**, the cursor is an item-creation timestamp — only items created after the cursor are emitted. For **PR and issue targets**, the cursor stores the last-delivered snapshot, so a restart re-diffs from that stored baseline and delivers everything that changed while offline (CI going red, reviews landing, comments arriving).

Without a named instance, every restart replays the full backlog as "New PR" — correct on first run but a flood on every subsequent restart.

```sh
# Named instances: each maintains its own independent cursor
gh monitor -R owner/repo --instance orchestrator    # resumes from its cursor
gh monitor -R owner/repo --instance agent-pr-957    # independent cursor
gh monitor -R owner/repo 42 --instance agent-pr-42  # PR monitoring with resume

# A brand-new instance starts at "now" — no pre-existing items are emitted
gh monitor -R owner/repo --instance fresh-watcher

# Opt into the backlog with --from-beginning
gh monitor -R owner/repo --instance fresh-watcher --from-beginning

# Manage cursors: list and reset
gh monitor instances list                # id, repo, cursor position, last seen
gh monitor instances reset orchestrator  # replay from the beginning next run
gh monitor instances reset --all         # delete every cursor
```

Cursors are independent — advancing one instance's cursor never affects another. The unnamed invocation (no `--instance`) keeps today's stateless behaviour exactly. Cursor files live under `~/.config/gh-monitor/instances/` and are written atomically (temp file + rename) so a crash mid-write leaves the previous state intact. Two processes using the same instance name observe last-writer-wins semantics.

### Shared poller daemon

By default every `gh monitor` process polls GitHub on its own `--interval` cadence. When many agents watch the same target (an orchestrator plus each agent watching the PR it owns), that is N independent fetches for the same data. The `daemon` command runs a long-lived process that maintains **one fetch loop per watched target** and fans each fetched snapshot out to every attached `monitor` client, so N processes share a single fetch ([#34](https://github.com/elecnix/gh-monitor/issues/34)).

```sh
# Start the daemon (runs until SIGTERM/SIGINT)
gh monitor daemon --interval 60

# Now any number of `gh monitor` clients watching the same PR share one fetch
gh monitor -R owner/repo 42
gh monitor -R owner/repo 42 --text
```

A `monitor` client detects the daemon via its Unix socket and streams from it instead of polling. When no daemon is running, `monitor` **auto-starts** one (a detached background process) and then connects, so you get shared polling without a manual `daemon` step. Each client keeps its **own baseline** snapshot, so consumption by one client never suppresses delivery to another — the core requirement behind [#32](https://github.com/elecnix/gh-monitor/issues/32).

Since [#76](https://github.com/elecnix/gh-monitor/issues/76) the daemon is the single watch code path: it multiplexes every target kind (pull requests, refs, commits, issues, workflow runs, whole repositories), and watching **requires** it — if no daemon can be attached, the client fails with an error naming the fix rather than silently polling in-process. The in-process polling loops were deleted. `--once` is the exception: it is a single fetch, answered in-process by the built-in backend (through the same one-shot hub path the daemon serves), so a one-shot read never spawns or requires a daemon.

Concurrent auto-starts are race-safe: at most one daemon binds a socket (a second `Listen` refuses to steal a live daemon's socket), so several clients starting at once share a single fetch loop rather than each spawning their own.

The daemon is **persistent once started** ([#69](https://github.com/elecnix/gh-monitor/issues/69)): it has no idle timeout and never exits for lack of attached clients. Auto-start only works if the daemon outlives the client that bootstrapped it — a fleet of short-lived watchers must not spawn a fresh daemon on every invocation. It serves until SIGTERM/SIGINT, or until a successor daemon completes an upgrade handoff.

#### Self-update

A resident daemon can also keep itself current. Set `selfUpdate` in the global preferences file (`gh monitor prefs set '{"selfUpdate": "30m"}'`) and the daemon periodically runs `gh extension upgrade gh-monitor` itself. The value is a Go duration (`"30m"`, `"2h"`), or `"1"`/`"true"` for the default hourly cadence; `""`, `"0"`, `"false"`, or null disables it (the default). When an upgrade lands, the existing handoff machinery takes over seamlessly — connected watchers ride across with no replay and no lost polling state.

Self-update is a **global-only setting** ([#82](https://github.com/elecnix/gh-monitor/issues/82)): it is read from the operator's `preferences.json` and nowhere else — not from any per-project config — because upgrading the installed binary is a machine-wide act that no single checkout should decide. A value that does not parse is rejected at set time rather than becoming a silent no-op, and the removed `GH_MONITOR_SELFUPDATE` environment variable is no longer honored: a daemon that still sees it set says so loudly and points at the new location.

It works identically whether or not sub-daemons are configured ([#84](https://github.com/elecnix/gh-monitor/issues/84), [#88](https://github.com/elecnix/gh-monitor/issues/88)): there is one daemon process owning the socket and the polling hub either way. An upgrade hands off seamlessly over the socket — watched targets and connected watchers ride across with no replay and no lost polling state — and the daemon relaunches its configured sub-daemon children on the new binary as it exits.

It only runs when the daemon was launched through its runtime copy (otherwise the installed file could not be rewritten anyway), and a failed check is logged and retried — never fatal, never disruptive to serving.

#### Poll cadence

The daemon poller's cadence is configurable from the global preferences file ([#90](https://github.com/elecnix/gh-monitor/issues/90)), so scheduled API spend can be tuned without restart-forgetting-flag discipline. The operating policy these settings express: **scheduled polling is a slow trickle that only exists as insurance against event loss** — timely updates arrive via a broker wake or a new subscriber's initial fetch, never from a timer.

```sh
gh monitor prefs set '{"pollInterval": "10m", "idlePollCeiling": "6h", "pollWhenBrokerHealthy": false}'
```

- `pollInterval` — the poller's base cadence, overriding `--interval`. A Go duration (`"10m"`), or `""`/`"0"`/`"false"` to keep the flag/built-in default (5 minutes as of [#90](https://github.com/elecnix/gh-monitor/issues/90)). This governs busy targets and the first few no-change polls.
- `idlePollCeiling` — caps the exponential idle backoff for every target, busy or quiet, broker-healthy or not, replacing the built-in 300s ceiling. A Go duration (`"6h"`).
- `pollWhenBrokerHealthy` — default `true`. Set to `false` and timer-driven fetching suspends entirely while the broker wake path reports healthy; the moment the broker degrades, every subscriber gets a loud `degraded` notice and an immediate fetch resumes (the transition itself wakes every poller). Requires the broker transport below.

All three are global-only settings beside `selfUpdate`, read at daemon start; invalid values are rejected at set time. `gh monitor prefs set` says so when a daemon-read key changes, and `gh monitor reload` applies the change immediately: it swaps in a successor daemon through the same in-memory handoff an upgrade uses, so watched targets, poller state, and connected watchers carry across — a reload, not a cold restart.

The socket path honours `$GH_MONITOR_SOCK`, then `$XDG_RUNTIME_DIR/gh-monitor.sock`, then `~/.cache/gh-monitor/daemon.sock`. Set `GH_MONITOR_AUTOSTART=0` to keep auto-start off (a client then fails with a clear error when no daemon is running, instead of spawning one). An explicitly configured external backend (`--backend-endpoint` / `$GH_MONITOR_BACKEND_ENDPOINT`) skips daemon attachment entirely — it is an authoritative operator choice for whatever kinds it declares.

#### Upgrading while watchers run

`gh extension upgrade` rewrites the installed binary in place, which Linux refuses with "text file busy" while any process has it mapped — and the daemon's whole job is to stay resident. Upgrades are therefore seamless end to end ([#73](https://github.com/elecnix/gh-monitor/issues/73)):

- **The installed file is never busy.** A resident command — the daemon, or a continuous `monitor` watch — launches from a runtime copy of the binary (`$XDG_RUNTIME_DIR/gh-monitor/gh-monitor`, falling back to the user cache dir) instead of the installed file, so the installed file is only ever exec'd for the instant of launch and the upgrade can rewrite it in place. Set `GH_MONITOR_REEXEC=0` to disable the relaunch and run from the installed path (the pre-#73 behaviour; upgrades then require stopping the daemon first).
- **The daemon adopts the new version by itself.** A resident daemon watches the installed binary; when an upgrade lands, it spawns the new binary, which performs an **in-memory handoff**: over the existing socket it receives the old daemon's watching state — every watched PR's last snapshot, backoff state, query tier, cached ruleset, and each connected watcher's baseline — and then the listening socket itself (passed as a file descriptor, so the socket path is never unbound). The old daemon exits cleanly; the new one serves in its place. No state file is written, and the takeover is seamless by construction:
  - **Watchers survive.** A connected `monitor` process sees its stream break, says so loudly once, reconnects to the same socket, and resumes from the baseline it was last shown — no second first-poll, no replay of what it already reported. Only what actually changed during the switchover is delivered.
  - **No polling state is lost.** The successor continues each PR's poll continuity instead of starting cold, so an upgrade does not re-fetch storm GitHub or re-notify watchers.
  - **No gap.** The successor serves on the adopted listening socket before the predecessor exits, so clients that probe the socket during the upgrade always find a daemon. A successor that fails to take over is announced loudly, and the old daemon keeps serving.

If the running daemon predates the handoff protocol, the new start fails with the ordinary "socket in use" error and clients keep using the old daemon — stop it by hand to complete that one upgrade. Watchers running a build older than the reconnect support still end their watch on a daemon change, as they always did.

#### Broker transport (optional)

The daemon can subscribe to an external GitHub-webhook fan-out broker (AWS IoT Core MQTT, or any broker publishing the same normalized event envelope) and treat each event as a wake signal that triggers an immediate fetch, instead of waiting for the next tick. It is entirely opt-in and additive: no `monitor` client, skill, or workflow changes — the daemon still fetches ground truth through the exact same GraphQL/REST path either way; a broker event only ever decides _when_ that fetch runs, never what it returns. A watcher never derives PR or CI state from the event stream itself, because that stream can drop messages across a long disconnect with no reliable replay.

Enable it by setting the endpoint before starting the daemon:

```sh
export GH_MONITOR_BROKER_ENDPOINT=your-iot-endpoint.iot.us-east-1.amazonaws.com
export GH_MONITOR_BROKER_TOPIC=github/+/+/+       # default; narrow to your org/repo if your broker's IAM policy is scoped
export GH_MONITOR_BROKER_REGION=us-east-1         # default
export GH_MONITOR_BROKER_IDLE_CAP=1800            # seconds; default 1800 (30m)

gh monitor daemon
```

The connection is authenticated the same way the AWS CLI is (`AWS_PROFILE`, an assumed role, etc.) via SigV4-presigned WebSocket credentials — nothing broker-specific to configure beyond the four variables above. The event envelope this transport expects:

```json
{
  "source": "github",
  "repository_owner": "my-org",
  "repository_name": "my-repo",
  "event_type": "pull_request",
  "pr_number": 42
}
```

**Health is loud, on purpose.** While the broker is connected, a quiet PR's idle-poll ceiling stretches from the default 300s up to `GH_MONITOR_BROKER_IDLE_CAP` — polling becomes a rare safety net because a real change now arrives as an immediate wake. The moment the connection drops, every subscriber gets a `degraded`-type notification and the ceiling reverts to the default within one poll cycle — normal interval polling, not silence. A subscriber that only ever reads "no event arrived" as "nothing changed" would be trading tonight's failure mode for a new transport instead of fixing it, so this transport is built to make that impossible: by default it always keeps polling underneath, just less often when the broker is doing its job. With `pollWhenBrokerHealthy: false` in preferences (see [Poll cadence](#poll-cadence)), that default becomes stricter still: while the wake path is live there is no timer polling at all, and polling returns the instant the transport degrades.

An event that names a repository but no `pr_number` (check-run/check-suite events, which key off a commit SHA rather than a PR) wakes every PR this daemon is currently watching for that repository, rather than guessing which one changed.

#### Sub-daemons (optional)

The daemon can launch and supervise **sub-daemon** processes configured by the operator. Each sub-daemon is a separate process — typically a backend that speaks the `backend/remote` protocol — that the daemon keeps alive with exponential backoff (reset after a stable run). The launcher is generic: it reads a list and launches processes. It does not know what a sub-daemon does or which protocol it speaks, so a proprietary backend can be started from this public repo without carrying any proprietary code.

**gh-monitor owns the socket; sub-daemons are routed sources** ([#88](https://github.com/elecnix/gh-monitor/issues/88)). Earlier releases (v1.19.0–v1.22.0) conceded `$GH_MONITOR_SOCK` to whichever sub-daemon bound it, which left every target kind that sub-daemon did not serve unservable. Now the daemon always binds the public socket itself and runs the polling hub; each child is launched with `GH_MONITOR_SOCK` pointed at a private per-entry socket (`<daemon-socket dir>/subdaemon-<name>.sock`, so no change is needed in the sub-daemon binary), the daemon discovers which kinds each live child serves from its protocol hello, and watches for those kinds are routed to the child while every other kind — and every resumable watch, whose history lives in the hub — is served by the polling hub on the public socket. A child that dies is restarted by the supervisor, and while it is down its kinds fall back to hub polling: degraded-but-covered instead of unmonitored.

Configure it with a line-delimited file, one sub-daemon per line (`<name> <executable> [args...]`, `#` comments and blank lines ignored, double-quoted fields may contain spaces):

```sh
# ~/.config/gh-monitor/daemons.conf
# Each line: <name> <executable> [args...]
broker-subscriber /usr/local/bin/broker-subscriber daemon --repo my-org/my-repo
```

The config path resolves in precedence order — an explicit env override, then a per-project file, then the operator's global config:

1. `$GH_MONITOR_SUBDAEMONS`, if set (an admin override)
2. `./.gh-monitor.conf` in the current working directory → a **project** can pin its own sub-daemons
3. `<user config dir>/gh-monitor/daemons.conf` (the global default)

Precedence is replacement, not merge: the first existing file is used whole. To pin a repository, drop a `daemons.conf`-format `.gh-monitor.conf` in its root. When no file exists, the daemon works exactly as it does today (pure polling). When the resolved file exists and lists at least one entry, the daemon launches the configured sub-daemons alongside the polling hub as described above. A sub-daemon that fails to start (binary not found) is logged once and not retried; one that crashes restarts with backoff, and after repeated rapid crashes the launcher gives up on that entry and continues with the rest.

```sh
# Override the config path
export GH_MONITOR_SUBDAEMONS=/etc/gh-monitor/daemons.conf
gh monitor daemon
```

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

The document shape is `{ "templates": {"<event-kind>": "<template>" | null}, "ignoredBots": ["login", …], "retriggerComments": false, "selfUpdate": "30m" | "1" | "" | null, "pollInterval": "10m" | "" | null, "idlePollCeiling": "6h" | "" | null, "pollWhenBrokerHealthy": true | false | null, "eventLog": {"dir": "/path", "keepDays": 10} | null }`. Event kinds and template tokens are listed in `gh monitor prefs --help`. A `--config-dir <dir>` flag overrides the config location (handy for testing).

#### Event log

Every backend event a watch consumes — from the built-in `gh` backend, the shared daemon, or an out-of-process broker sub-daemon — can be recorded to daily append-only JSONL files ([#86](https://github.com/elecnix/gh-monitor/issues/86)). Logging happens above the backend layer: each line is the raw update envelope exactly as the backend delivered it, before rendering and before notification preferences apply.

```sh
gh monitor prefs set '{"eventLog": {}}'                          # on, defaults
gh monitor prefs set '{"eventLog": {"dir": "/var/log/gh-monitor", "keepDays": 30}}'
```

Both fields are optional: `dir` defaults to the user cache dir's `gh-monitor/events`, and `keepDays` defaults to 10. Rotation is a filename change (`events-YYYY-MM-DD.jsonl`), never a rewrite — a new day means a new file, and files older than the retention window are pruned when the day rolls over. It is off by default; a write failure disables logging for that watch with one loud line and never interrupts watching.

### Checking which build is running

After an upgrade, ask the binary itself:

```sh
gh monitor --version
gh monitor version      # same output
```

```
gh monitor v1.17.0 (55d9154, 2026-08-11T15:24:54Z, go1.22.12)
```

`gh extension list` reports the install **manifest** instead. That is whatever the installer wrote there; it is not derived from the executable and does not change if the executable is replaced by a local build, a partially-applied upgrade, or a manual copy. `--version` reads the running binary's own stamp, so it is the one that answers "did my upgrade actually land?".

Reading the output:

- The version is stamped at release time. A locally built binary prints `(devel)` rather than a tag.
- The revision is the short commit the binary was built from, suffixed `-dirty` when the working tree was not clean.
- `unknown revision` means the build stripped its VCS metadata — `go build -buildvcs=false`, which is what a build inside a linked worktree needs.

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
