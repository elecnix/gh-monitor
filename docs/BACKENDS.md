# Backends

`gh monitor` watches GitHub through a **backend**. The built-in one polls the
GitHub API. You can supply your own — as a Go library or as a separate process —
and it will take over as much or as little of the job as it implements.

## The two capabilities

A backend provides one or both of these. They are independent, and a backend
registers only what it actually has.

| Capability | Interface        | What it does                                           | Used by                  |
| ---------- | ---------------- | ------------------------------------------------------ | ------------------------ |
| `source`   | `backend.Source` | Delivers `Update`s describing what changed on a target | continuous watching      |
| `reader`   | `backend.Reader` | Returns a target's current `Status`                    | `--once`, and any poller |

The split matters because the two jobs have different answers. A backend that
learns about changes as they happen has a much better `Source` than polling can
be, and no reason to reimplement reads — so it registers a `Source`, omits the
`Reader`, and `--once` keeps using the built-in backend.

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
BACKEND  CAPABILITIES   KINDS
gh       source,reader  all
relay    source         pr,run
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

| Field      | Meaning                                                                                                                                     |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `Since`    | An opaque cursor from an earlier `Update`. Empty means start from now.                                                                      |
| `Kinds`    | The event types the caller cares about. Empty means all. Delivering more is fine; the caller filters again.                                 |
| `Interval` | The caller's preferred cadence. Meaningful to a poller, meaningless otherwise.                                                              |
| `Timeout`  | Stop after this long. Zero means run until terminal or cancelled.                                                                           |
| `Once`     | Deliver the current actionable state, then close — do not keep watching. If you cannot tell the two apart, emit what is true now and close. |

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
		Type:            backend.EventDegraded,
		DegradedSurface: "upstream",
		DegradedMessage: err.Error(),
	},
	At: time.Now(),
}
```

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
| `{"done":true}`    | End of stream. The watch finished.                          |
| `{"error":"..."}`  | End of stream with a failure; reported as a degraded event. |

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
		// Reader omitted: reads stay with the built-in backend.
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

| Kind                                                                   | Meaning                                  |
| ---------------------------------------------------------------------- | ---------------------------------------- |
| `first-poll`                                                           | Baseline: what is being watched          |
| `new-failing-checks`, `ci-all-green`, `check-annotations`              | CI outcomes                              |
| `new-unresolved-threads`, `new-general-comments`                       | Review activity                          |
| `review-approved`, `review-changes-requested`, `review-dismissed`      | Review decisions                         |
| `new-commit`, `conflict`, `merged`, `closed`                           | Pull request state                       |
| `issue-closed`, `issue-reopened`, `issue-new-comment`, `issue-mention` | Issue state                              |
| `run-queued`, `run-in-progress`, `run-completed`                       | Workflow runs                            |
| `repo-new-pr`, `repo-new-issue`, `readiness`                           | Repository                               |
| `degraded`                                                             | A surface could not be read              |
| `all-clear`                                                            | Everything previously raised is resolved |

## What the built-in backend still owns

The `gh` backend keeps the parts that are specific to polling the GitHub API,
and no other backend has to think about them:

- adaptive idle backoff and jitter, so quiet targets cost less and concurrent
  watchers do not poll in phase
- GraphQL budget awareness and query tiering, shedding the least valuable
  surfaces first and saying loudly what it stopped watching
- the shared-poller daemon (`gh monitor daemon`), which multiplexes one fetch
  per pull request across several `gh monitor` processes

The daemon is used only when the built-in backend is what would otherwise do the
polling. With an external backend serving a target, `gh monitor` streams from
that backend directly.
