# Backends

`gh monitor` watches GitHub through a **backend**. The built-in one polls the
GitHub API. You can supply your own — as a Go library or as a separate process —
and it will take over as much or as little of the job as it implements.

## The capabilities

A backend provides any subset of these. They are independent, and a backend
registers only what it actually has.

| Capability  | Interface               | What it does                                           | Used by               |
| ----------- | ----------------------- | ------------------------------------------------------ | --------------------- |
| `source`    | `backend.Source`        | Delivers `Update`s describing what changed on a target | continuous watching   |
| `reader`    | `backend.Reader`        | Returns a target's current `Status`                    | `--once`              |
| `threads`   | `backend.ThreadActor`   | Lists, views, resolves review threads                  | `gh monitor threads`  |
| `review`    | `backend.ReviewActor`   | Drives a pending review                                | `gh monitor review`   |
| `comments`  | `backend.CommentActor`  | Replies to review threads                              | `gh monitor comments` |
| `draft`     | `backend.DraftActor`    | Reads and changes draft status                         | `gh monitor draft`    |
| `reactions` | `backend.ReactionActor` | Adds a reaction to a node                              | `gh monitor react`    |

The split matters because the jobs have different answers. A backend that
learns about changes as they happen has a much better `Source` than polling can
be, and no reason to reimplement anything else — so it registers a `Source`,
omits the rest, and every other verb keeps using the built-in backend.

Nothing forces a backend to take a capability it does not want. Registering
`threads` alone is a complete, valid backend.

## Target kinds

A backend also declares which kinds of target it covers:

`pr`, `issue`, `run`, `ref`, `commit`, `repo`

Resolution is per kind, most specific first: a backend that names its kinds wins
for those kinds, and everything else falls to a backend registered for every
kind — normally the built-in `gh`. So covering pull requests and workflow runs
does not leave issues unwatched; they simply stay with the poller.

Among equally specific registrations, the most recent one wins. `--backend
<name>` pins resolution to one backend and fails loudly if that backend does not
cover the target, rather than quietly falling back to something you did not ask
for.

Check what is registered:

```sh
gh monitor backends
gh monitor backends --json
```

```
BACKEND  CAPABILITIES                                        KINDS
gh       source,reader,threads,review,comments,draft,reactions  all
relay    source,threads                                      pr,run
```

## Writing a backend as a Go library

Import `github.com/elecnix/gh-monitor/backend` and implement `Source`, `Reader`,
or both. The package has no GitHub API types in it, so an out-of-tree module can
depend on it without pulling in this one's internals.

```go
package main

import (
	"context"

	"github.com/elecnix/gh-monitor/backend"
)

type mySource struct{}

func (s *mySource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	out := make(chan backend.Update)
	go func() {
		defer close(out)
		for change := range myChanges(ctx, t) {
			out <- backend.Update{
				ID:     change.DeliveryID, // repeats are dropped by ID
				Target: t,
				Event: backend.Event{
					Type:   backend.EventNewFailingChecks,
					Checks: change.FailedCheckNames,
				},
				At: change.At,
			}
		}
	}()
	return out, nil
}
```

Register it and build your own binary:

```go
reg := backend.NewRegistry()
reg.RegisterSource("mine", []backend.Kind{backend.KindPR}, &mySource{})
```

Each capability has its own `Register` method — `RegisterSource`,
`RegisterReader`, `RegisterThreads`, `RegisterReview`, `RegisterComments`,
`RegisterDraft`, `RegisterReactions`. Call the ones you implement.

### What an Update carries

| Field      | Meaning                                                                                                                                                    |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ID`       | Stable identity. A repeat of an ID already seen is dropped, so at-least-once delivery does not notify twice. Empty disables deduplication for that update. |
| `Target`   | What the update is about.                                                                                                                                  |
| `Event`    | The change: a type from the shared vocabulary, plus the fields relevant to it.                                                                             |
| `Status`   | The target's state at observation time. Optional — leave it nil if you do not have one.                                                                    |
| `Cursor`   | Opaque resume token; a caller may hand it back as `WatchOptions.Since`.                                                                                    |
| `At`       | When the change was observed.                                                                                                                              |
| `Terminal` | The target is finished — merged, closed, completed. The channel closes after it.                                                                           |

You do not render messages. `gh monitor` turns every `Update` into a
notification using the user's own templates and `--events` filter, so your
backend's output looks exactly like the built-in one's, and `gh monitor prefs`
still controls it.

### What WatchOptions asks for

Every field is advisory. Ignore what you cannot honour rather than failing —
except `Once`, which changes what the caller is asking for.

| Field              | Meaning                                                                                                                                     |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `Since`            | An opaque cursor from an earlier `Update`. Empty means start from now.                                                                      |
| `Kinds`            | The event types the caller cares about. Empty means all. Delivering more is fine; the caller filters again.                                 |
| `Interval`         | The caller's preferred cadence. Meaningful to a poller, meaningless otherwise.                                                              |
| `Timeout`          | Stop after this long. Zero means run until terminal or cancelled.                                                                           |
| `Once`             | Deliver the current actionable state, then close — do not keep watching. If you cannot tell the two apart, emit what is true now and close. |
| `IgnoredAuthors`   | Drop activity by these logins before reporting it.                                                                                          |
| `AnnotationLevels` | Which check-annotation severities to report. Empty means your default; `["none"]` means report none.                                        |
| `RepeatUnresolved` | Re-report still-open items on every observation, not only when they first appear.                                                           |

### Say so when you go blind

A `Source` that stops seeing its target must emit an `EventDegraded` update
saying why. A watcher that simply goes quiet is indistinguishable from a target
where nothing is happening, and a monitor that cannot tell those apart is worse
than no monitor. Return an error from `Watch` only when the watch could not be
started at all; report everything after that as a degraded update.

```go
out <- backend.Update{
	Target: t,
	Event: backend.Event{
		Type:             backend.EventDegraded,
		DegradedSurface:  "upstream",
		DegradedMessage:  err.Error(),
		DegradedSurfaces: []string{"check outcomes", "comments"},
	},
	At: time.Now(),
}
```

outcomes even though the tier system never sheds them. The list reaches
callers both as a structured field and in the rendered sentence; leave it
empty when a backend has no per-surface claims to make.

When the surface recovers, declare the gap
([#99](https://github.com/elecnix/gh-monitor/issues/99)): set
`DegradedFrom` and `DegradedTo` (RFC 3339) on the recovery notice to the
blind window's boundaries, and say in the notice that events between them
were not observed. The cursor contract never replays — a cursor advances
only on successful fetches — so without the declaration, what a caller
missed stays invisible to it forever.

## Running a backend as a server

A backend can also run as a separate process. `gh monitor` connects to it and
speaks line-delimited JSON, so the backend can be written in any language.

Point `gh monitor` at it with `--backend-endpoint` or `$GH_MONITOR_BACKEND`:

```sh
gh monitor --backend-endpoint unix:/run/my-backend.sock 42
gh monitor --backend-endpoint tcp:127.0.0.1:9000 42
gh monitor --backend-endpoint 'exec:my-backend --verbose' 42

export GH_MONITOR_BACKEND=unix:/run/my-backend.sock
gh monitor 42
```

`exec:` runs the command and talks over its stdin/stdout; its stderr passes
through to yours, so the backend's own diagnostics stay visible. Each watch
opens its own connection.

A malformed endpoint fails at startup rather than falling back, so you are never
left believing an external backend is watching when it is not.

The handshake is bounded: a peer that does not send its hello within two
seconds is abandoned. Without that bound, pointing `gh monitor` at something
that is not a backend — including a `gh monitor daemon` from a build before
this protocol, which waits for the client to speak first — would block forever
with no output and no error.

### The protocol

On connect the server announces itself, the client sends one request, and the
server streams the answer.

```
server → {"protocol":1,"name":"relay","capabilities":["source"],"kinds":["pr","run"]}
client → {"op":"watch","target":{"kind":"pr","owner":"o","repo":"r","number":42},"options":{}}
server → {"update":{"target":{...},"event":{"type":"new-failing-checks","checks":["build"]},"at":"..."}}
server → {"update":{"target":{...},"event":{"type":"merged"},"terminal":true,"at":"..."}}
server → {"done":true}
```

The hello is where partial registration happens: `capabilities` and `kinds` are
what the server claims, and `gh monitor` registers exactly that. Omit `kinds` to
claim every kind — a strong claim, since everything will then be routed to you.

| Frame              | Meaning                                                     |
| ------------------ | ----------------------------------------------------------- |
| `{"update":{...}}` | One change.                                                 |
| `{"status":{...}}` | The answer to `{"op":"read"}`.                              |
| `{"result":{...}}` | The answer to a mutation op.                                |
| `{"done":true}`    | End of stream. The watch finished.                          |
| `{"error":"..."}`  | End of stream with a failure; reported as a degraded event. |

### Mutation ops

Mutations use the same request/response shape: one request in, one frame out.
The arguments go in `payload`, the return value comes back in `result`.

```
client → {"op":"threads.resolve","target":{...},"payload":{"ThreadID":"PRRT_1"}}
server → {"result":{"thread_node_id":"PRRT_1","is_resolved":true}}
```

| Capability  | Ops                                                                                                  |
| ----------- | ---------------------------------------------------------------------------------------------------- |
| `threads`   | `threads.list`, `threads.view`, `threads.resolve`, `threads.unresolve`                               |
| `review`    | `review.start`, `review.addComment`, `review.updateComment`, `review.deleteComment`, `review.submit` |
| `comments`  | `comments.reply`                                                                                     |
| `draft`     | `draft.status`, `draft.set`, `draft.list`                                                            |
| `reactions` | `reactions.react`                                                                                    |

An op for a capability the server did not declare comes back as an `error`
frame rather than a zero value, so a caller never mistakes "not implemented"
for "nothing to do".

A stream that ends without `done` or `error` is treated as a failure and
surfaced as a degraded event — the client will not read a dropped connection as
"nothing is happening".

### Serving it from Go

`backend/remote` has the server side, so a Go backend is an accept loop:

```go
package main

import (
	"context"
	"net"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
)

func main() {
	ln, err := net.Listen("unix", "/run/my-backend.sock")
	if err != nil {
		panic(err)
	}
	cfg := remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR, backend.KindRun},
		Source: &mySource{},
		// Every other field is optional. What you leave nil stays with the
		// built-in backend, and is not announced in the hello.
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = remote.Serve(context.Background(), conn, cfg)
		}()
	}
}
```

## The event vocabulary

Whatever a backend learned, it says it in these terms. They are the same kinds
`--events` filters on and `gh monitor prefs` templates.

| Kind                                                                   | Meaning                                                                                                                                                                                 |
| ---------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `first-poll`                                                           | Baseline: what is being watched                                                                                                                                                         |
| `new-failing-checks`, `ci-all-green`, `check-annotations`              | CI outcomes                                                                                                                                                                             |
| `new-unresolved-threads`, `new-general-comments`                       | Review activity                                                                                                                                                                         |
| `review-approved`, `review-changes-requested`, `review-dismissed`      | Review decisions                                                                                                                                                                        |
| `new-commit`, `conflict`, `merged`, `closed`                           | Pull request state                                                                                                                                                                      |
| `issue-closed`, `issue-reopened`, `issue-new-comment`, `issue-mention` | Issue state                                                                                                                                                                             |
| `run-queued`, `run-in-progress`, `run-completed`                       | Workflow runs                                                                                                                                                                           |
| `repo-new-pr`, `repo-new-issue`, `readiness`                           | Repository                                                                                                                                                                              |
| `degraded`                                                             | A surface could not be read — emitted per episode (entering degraded, error change, recovery), not per failed poll. Names what the failed read stopped delivering (`degraded_surfaces`) |
| `all-clear`                                                            | Everything previously raised is resolved                                                                                                                                                |

## The shared-poller daemon

`gh monitor daemon` is a backend like any other. It speaks the protocol above,
announcing itself as `daemon` with a single capability (`source`), because
multiplexing one fetch across several watchers is all it does. Since
[#76](https://github.com/elecnix/gh-monitor/issues/76) it covers every target
kind — one poller per watched identity, whatever the kind — and it is the only
way to watch continuously: the built-in backend's Source serves one-shot reads
only, so a watch with no daemon attached is a hard error, never a silent
in-process poll loop. Reads and mutations resolve past it to the built-in
backend.

That is also why it is not special-cased in the client. It registers after the
built-in backend and before any backend you configured, so an external backend
you asked for still wins — and configuring one skips daemon attachment
entirely.

```sh
gh monitor --backend daemon 42   # pin to the shared poller
```

## What the built-in backend still owns

The `gh` backend provides every capability for every kind, so it is the
fallback under anything partial. Its Source serves one-shot reads (`--once`):
a single fetch through an in-process hub, no daemon required. It also keeps
the parts that are specific to polling the GitHub API, which no other backend
has to think about:

- adaptive idle backoff and jitter, so quiet targets cost less and concurrent
  watchers do not poll in phase (the shared-poller daemon applies the same
  cadence model per poller)
- GraphQL budget awareness and query tiering, shedding the least valuable
  surfaces first and saying loudly what it stopped watching
