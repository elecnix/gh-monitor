# Output schemas

Optional fields are omitted entirely (never serialized as `null`). Unless noted,
schemas disallow additional properties to surface unexpected payload changes.

## ReviewState

Used by `review --start` and `review --submit`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ReviewState",
  "type": "object",
  "required": ["id", "state"],
  "properties": {
    "id": {
      "type": "string",
      "description": "review node identifier (PRR_…)"
    },
    "state": {
      "type": "string",
      "enum": [
        "PENDING",
        "COMMENTED",
        "APPROVED",
        "DISMISSED",
        "REQUEST_CHANGES"
      ]
    },
    "submitted_at": {
      "type": "string",
      "format": "date-time",
      "description": "RFC3339 timestamp of the submission (omitted when pending)"
    }
  },
  "additionalProperties": false
}
```

## ReviewThread

Produced by `review --add-comment`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ReviewThread",
  "type": "object",
  "required": ["id", "path", "is_outdated"],
  "properties": {
    "id": {
      "type": "string",
      "description": "review thread node identifier"
    },
    "path": {
      "type": "string",
      "description": "File path for the inline thread"
    },
    "is_outdated": {
      "type": "boolean"
    },
    "line": {
      "type": "integer",
      "minimum": 1,
      "description": "Updated diff line (omitted for multi-line threads)"
    }
  },
  "additionalProperties": false
}
```

## ReviewReport

Emitted by `review view`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ReviewReport",
  "type": "object",
  "required": ["reviews"],
  "properties": {
    "reviews": {
      "type": "array",
      "items": {
        "$ref": "#/$defs/ReportReview"
      }
    }
  },
  "additionalProperties": false,
  "$defs": {
    "ReportReview": {
      "type": "object",
      "required": ["id", "state", "author_login"],
      "properties": {
        "id": {
          "type": "string"
        },
        "state": {
          "type": "string",
          "enum": ["APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED"]
        },
        "body": {
          "type": "string"
        },
        "submitted_at": {
          "type": "string",
          "format": "date-time"
        },
        "author_login": {
          "type": "string"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/ReportComment"
          }
        }
      },
      "additionalProperties": false
    },
    "ReportComment": {
      "type": "object",
      "required": [
        "thread_id",
        "path",
        "author_login",
        "body",
        "created_at",
        "is_resolved",
        "is_outdated",
        "thread_comments"
      ],
      "properties": {
        "thread_id": {
          "type": "string",
          "description": "review thread identifier"
        },
        "comment_node_id": {
          "type": "string",
          "description": "comment node identifier when requested"
        },
        "path": {
          "type": "string"
        },
        "line": {
          "type": ["integer", "null"],
          "minimum": 1
        },
        "author_login": {
          "type": "string"
        },
        "body": {
          "type": "string"
        },
        "created_at": {
          "type": "string",
          "format": "date-time"
        },
        "is_resolved": {
          "type": "boolean"
        },
        "is_outdated": {
          "type": "boolean"
        },
        "thread_comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/ThreadReply"
          }
        }
      },
      "additionalProperties": false
    },
    "ThreadReply": {
      "type": "object",
      "required": ["id", "author_login", "body", "created_at"],
      "properties": {
        "comment_node_id": {
          "type": "string",
          "description": "comment node identifier when requested"
        },
        "author_login": {
          "type": "string"
        },
        "body": {
          "type": "string"
        },
        "created_at": {
          "type": "string",
          "format": "date-time"
        }
      },
      "additionalProperties": false
    }
  }
}
```

## ReplyMinimal

Returned by `comments reply`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ReplyMinimal",
  "type": "object",
  "required": ["comment_node_id"],
  "properties": {
    "comment_node_id": {
      "type": "string",
      "description": "comment node identifier"
    }
  },
  "additionalProperties": false
}
```

## StatusResult

Returned by `review --edit-comment`, `review --delete-comment`, and `review --submit`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "StatusResult",
  "type": "object",
  "required": ["status"],
  "properties": {
    "status": {
      "type": "string",
      "description": "operation status message"
    }
  },
  "additionalProperties": false
}
```

## ThreadSummary

Returned by `threads list`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ThreadSummary",
  "type": "object",
  "required": ["thread_id", "is_resolved", "path", "is_outdated"],
  "properties": {
    "thread_id": {
      "type": "string"
    },
    "is_resolved": {
      "type": "boolean"
    },
    "resolved_by": {
      "type": "string",
      "description": "Login of the user who resolved the thread"
    },
    "updated_at": {
      "type": "string",
      "format": "date-time"
    },
    "path": {
      "type": "string"
    },
    "line": {
      "type": "integer",
      "minimum": 1
    },
    "is_outdated": {
      "type": "boolean"
    }
  },
  "additionalProperties": false
}
```

## ThreadMutationResult

Returned by `threads resolve` and `threads unresolve`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ThreadMutationResult",
  "type": "object",
  "required": ["thread_node_id", "is_resolved"],
  "properties": {
    "thread_node_id": {
      "type": "string"
    },
    "is_resolved": {
      "type": "boolean"
    }
  },
  "additionalProperties": false
}
```

## ReactionResult

Returned by `react`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ReactionResult",
  "type": "object",
  "required": ["node_id", "reaction", "status"],
  "properties": {
    "node_id": {
      "type": "string",
      "description": "GraphQL node ID of the reacted object"
    },
    "reaction": {
      "type": "string",
      "description": "Reaction type (thumbs_up, thumbs_down, laugh, hooray, confused, heart, rocket, eyes)"
    },
    "status": {
      "type": "string",
      "enum": ["added"]
    }
  },
  "additionalProperties": false
}
```

## MonitorNotification

Emitted by `monitor` as one NDJSON object per line (one event per genuinely-new change). Fields other than the core four are present only when relevant to the event `type`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "MonitorNotification",
  "type": "object",
  "required": ["type", "pr_label", "message", "timestamp"],
  "properties": {
    "type": {
      "type": "string",
      "enum": [
        "first-poll",
        "new-failing-checks",
        "ci-all-green",
        "new-unresolved-threads",
        "new-general-comments",
        "conflict",
        "review-approved",
        "review-changes-requested",
        "review-dismissed",
        "new-commit",
        "merged",
        "closed",
        "issue-closed",
        "issue-reopened",
        "issue-new-comment",
        "issue-mention",
        "run-queued",
        "run-in-progress",
        "run-completed",
        "repo-new-pr",
        "repo-new-issue"
      ],
      "description": "The kind of event"
    },,
    "pr_label": {
      "type": "string",
      "description": "owner/repo#number for PR/issue targets; owner/repo for repo targets (with item labels as owner/repo#N); owner/repo@ref for ref/commit; owner/repo run #N for workflow runs"
    },
    "message": {
      "type": "string",
      "description": "Rendered notification text (from the templates in preferences.json)"
    },
    "unresolved_threads": {
      "type": "integer",
      "description": "Count of currently-unresolved review threads (omitted when 0)"
    },
    "general_comments": {
      "type": "integer",
      "description": "Count of currently-actionable general comments (omitted when 0)"
    },
    "failing_checks": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Newly-failing check names (new-failing-checks)"
    },
    "commit_short_oid": {
      "type": "string",
      "description": "Short SHA of the new head commit (new-commit)"
    },
    "commit_author": {
      "type": "string",
      "description": "Author of the new head commit (new-commit)"
    },
    "review_author": {
      "type": "string",
      "description": "Author of the review decision (review-* events)"
    },
    "detail": {
      "type": "string",
      "description": "Rich, self-contained body with per-thread/comment excerpts and act-on-it hints (new-unresolved-threads / new-general-comments), or the last 50 lines of the failed-job logs for a failed run-completed (`gh run view <run-id> --log-failed`). A truncated snippet opens with a one-line marker stating how many earlier lines were dropped and the exact command that fetches the full failed-step log (`gh run view <run-id> --repo <owner>/<repo> --log-failed`); lets a consumer act without extra API calls"
    },
    "pr_url": {
      "type": "string",
      "description": "PR web URL, used for OSC-8 links in --text output"
    },
    "commit_url": {
      "type": "string",
      "description": "Commit web URL, used for the OSC-8 link on the short SHA (new-commit)"
    },
    "run_id": {
      "type": "integer",
      "description": "GitHub Actions workflow run id (run-* events)"
    },
    "conclusion": {
      "type": "string",
      "enum": ["success", "failure", "timed_out", "cancelled", "neutral", "action_required", "stale", "skipped"],
      "description": "Terminal conclusion of a workflow run (run-completed)"
    },
    "repo_items": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["number", "title", "author", "url"],
        "properties": {
          "number": { "type": "integer" },
          "title": { "type": "string" },
          "author": { "type": "string" },
          "url": { "type": "string" }
        }
      },
      "description": "New PRs or issues detected (repo-new-pr, repo-new-issue)"
    },
    "timestamp": {
      "type": "string",
      "format": "date-time",
      "description": "When the event was emitted"
    }
  }
}
```
