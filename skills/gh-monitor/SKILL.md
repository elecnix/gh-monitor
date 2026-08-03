---
name: gh-monitor
description: View, reply, and resolve inline GitHub PR review comments from the terminal. Monitor PRs for new comments, CI failures, conflicts, and other events — built for coding agents.
---

# gh-monitor

A GitHub CLI extension built for coding agents (Claude Code, pi, etc.) — inline PR review comments, thread management, live monitoring with streaming NDJSON events, and structured JSON output.

## When to Use

Use when you need to interact with a PR: view/reply/resolve review threads, create reviews with inline comments, manage draft status, add reactions, or continuously monitor for events (`monitor`). Use `monitor --once` for a one-shot check.

## Installation

```sh
gh extension install elecnix/gh-monitor
```

## Core Commands

All commands accept `-R owner/repo`. IDs use GraphQL format (`PRR_*`, `PRRT_*`, `PRRC_*`). Use `--body-file -` for stdin input.

### View Reviews & Threads

```sh
gh monitor review view -R owner/repo --pr <number>
```

Key filters: `--unresolved`, `--not_outdated`, `--tail 1`, `--reviewer <login>`, `--include-comment-node-id`.

### Reply to Threads

```sh
gh monitor comments reply <pr-number> -R owner/repo --thread-id <id> --body "..."
```

### List / View / Resolve Threads

```sh
gh monitor threads list -R owner/repo <pr> --unresolved --mine
gh monitor threads view <thread_id> [<thread_id> ...]
gh monitor threads resolve -R owner/repo <pr> --thread-id <id>
gh monitor threads unresolve -R owner/repo <pr> --thread-id <id>
```

### Reactions

```sh
gh monitor react <comment_id> --type thumbs_up
```

Valid types: `thumbs_up`, `thumbs_down`, `laugh`, `hooray`, `confused`, `heart`, `rocket`, `eyes`.

### Draft Status

```sh
gh monitor draft status -R owner/repo <pr>
gh monitor draft mark -R owner/repo <pr>
gh monitor draft ready -R owner/repo <pr>
gh monitor draft list -R owner/repo
```

### Create & Submit Reviews

Start a pending review, add inline comments, then submit:

```sh
gh monitor review --start -R owner/repo <pr>
gh monitor review --add-comment -R owner/repo <pr> --review-id <id> --path <file> --line <n> --body "..."
gh monitor review --submit -R owner/repo <pr> --review-id <id> --event <APPROVE|REQUEST_CHANGES|COMMENT> --body "..."
```

Edit or delete pending comments: `--edit-comment` / `--delete-comment` with `--comment-id`. Pin start to a commit with `--commit <sha>`.

### Monitor a PR

Monitor is the default command — just pass the PR selector directly. Continuously stream one NDJSON event per genuinely-new change until merge/close:

```sh
gh monitor -R owner/repo <pr>            # Default command
gh monitor --text -R owner/repo <pr>     # Human-readable
gh monitor --once -R owner/repo <pr>     # One-shot, then exit
```

Flags: `--interval` (default 60s, min 10), `--timeout` (default 0 = forever), `--ignored-bots <a,b>`.

**Event types:**

| Type                       | Description             |
| -------------------------- | ----------------------- |
| `new-unresolved-threads`   | New review threads      |
| `new-general-comments`     | New general PR comments |
| `new-failing-checks`       | Newly failing CI        |
| `ci-all-green`             | All CI passing          |
| `conflict`                 | Merge conflicts         |
| `review-approved`          | PR approved             |
| `review-changes-requested` | Changes requested       |
| `review-dismissed`         | Review dismissed        |
| `new-commit`               | New commit pushed       |
| `merged`                   | PR merged               |
| `closed`                   | PR closed               |

**Claude Code integration:** Wrap in a persistent `Monitor` tool — each NDJSON line becomes a session notification:

```
Monitor({ command: "gh monitor -R owner/repo 42", persistent: true })
```

**Adaptive backoff:** After 3 no-change polls, interval grows exponentially (cap 5min), resets on any change. Transient errors retry with doubling backoff — the loop doesn't crash.

**One-shot check:** `gh monitor --once -R owner/repo <pr>` emits current actionable state and exits (replaces the removed `await` command).

### Monitor a workflow run

Watch a **single GitHub Actions workflow run** until it completes — for any non-PR run (deploy workflows on `main`, `workflow_dispatch`, scheduled runs). The run id is the numeric id in `…/actions/runs/<id>` (`databaseId` from `gh run list`). The loop auto-stops when the run's status becomes `completed`.

```sh
gh monitor --run-id <id> -R owner/repo          # Watch until complete
gh monitor --run-id <id> -R owner/repo --once  # One-shot state
gh monitor --run-id <id> -R owner/repo --text  # Human-readable
```

Same flags as PR monitoring (`--interval`, `--timeout`, `--once`, `--text`, `-R`). `--run-id` is mutually exclusive with the PR selector and `--ref`/`--commit`/`--issue`.

**Run event types:**

| Type              | Description                                           |
| ----------------- | ----------------------------------------------------- |
| `run-queued`      | Run transitioned to `queued`                          |
| `run-in-progress` | Run transitioned to `in_progress`                     |
| `run-completed`   | Run finished — `conclusion` field carries the outcome |

The `run-completed` event's `conclusion` is one of: `success`, `failure`, `timed_out`, `cancelled`, `neutral`, `action_required`, `stale`, `skipped`. Each event includes `run_id`, the run URL (`pr_url`), and the head commit. For a failure conclusion (`failure`, `timed_out`, `cancelled`, `action_required`), the event also includes a `detail` body with the first 50 lines of the failed-job logs (`gh run view <run-id> --log-failed`), so an agent can diagnose without an extra API call.

## Critical Workflows

### Claim a Thread Before Working on It

Add an 👀 reaction to the **first** comment in a thread to signal you're working on it — prevents duplicate effort from other agents:

```sh
gh monitor react <comment_node_id> --type eyes
```

A reaction does NOT resolve a thread. After addressing, reply then resolve with `threads resolve`.

### Reply to All Unresolved Comments

1. List: `gh monitor threads list --unresolved -R owner/repo <pr>`
2. Reply: `gh monitor comments reply <pr> -R owner/repo --thread-id <id> --body "..."`
3. Resolve: `gh monitor threads resolve <pr> -R owner/repo --thread-id <id>`

If non-actionable, resolve with a brief reply explaining why.

### Create Review with Inline Comments

1. `gh monitor review --start -R owner/repo <pr>`
2. `gh monitor review --add-comment ...`
3. `gh monitor review --submit -R owner/repo <pr> --review-id <id> --event REQUEST_CHANGES --body "Summary"`

### Get Actionable Unresolved Comments

```sh
gh monitor review view --unresolved --not_outdated --tail 1 -R owner/repo --pr $(gh pr view --json number -q .number)
```

## Key Notes

- Comments can only be edited/deleted while the review is pending (before submit).
- Numeric review IDs are rejected — always use GraphQL node IDs (`PRR_*`).
- Monitor notification templates are overridable via `${XDG_CONFIG_HOME:-~/.config}/gh-monitor/preferences.json`.
- All commands return structured JSON with consistent field names, no nulls, pre-joined thread replies.

## References

- [docs/SCHEMAS.md](../../docs/SCHEMAS.md) — JSON schemas
- [skills/references/USAGE.md](../references/USAGE.md) — Detailed usage
- [docs/CLAUDE_CODE.md](../../docs/CLAUDE_CODE.md) — Claude Code integration guide
- Fork of [agynio/gh-pr-review](https://github.com/agynio/gh-pr-review).
