package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/orphan"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonitorOrphanGuardCancelsTheWatch pins the issue-#129 contract: when
// the process that launched a watch exits, the watch must end. The guard's
// Done channel is wired into the watch context, so a reparented monitor stops
// polling instead of running until its target merges.
func TestMonitorOrphanGuardCancelsTheWatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	// A source whose watch never delivers and never closes on its own: the
	// only thing that can end this watch is the orphan guard.
	src := &blockingSource{watched: make(chan backend.Target, 1)}
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR},
		Source: src,
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	// Swap the guard for one whose poller reports a changed parent after the
	// watch starts: the fake reparenting a launcher's exit produces.
	realMaybeStart := maybeStartGuardFn
	defer func() { maybeStartGuardFn = realMaybeStart }()
	currentPPID := os.Getppid()
	fired := make(chan struct{})
	maybeStartGuardFn = func() *orphan.Guard {
		return orphan.New(orphan.DefaultInterval, func() int {
			select {
			case <-fired:
				return currentPPID + 1
			default:
				return currentPPID
			}
		}).Start()
	}

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	// The guard starts before the watch is requested, so it is safe to fire
	// once the source has seen the watch.
	select {
	case <-src.watched:
	case <-time.After(10 * time.Second):
		t.Fatal("watch never started")
	}
	close(fired)

	select {
	case err := <-done:
		// A watch the orphan guard ended is a cancelled watch, not a failure:
		// nil, the same contract as Ctrl-C.
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end after the launcher exited")
	}
}

// TestMonitorWriteFailureEndsTheWatch pins the EPIPE contract: when the
// consumer of the watch's output is gone (a closed pipe), the first failed
// write ends the watch instead of being swallowed. A monitor whose reader has
// exited must not keep polling.
func TestMonitorWriteFailureEndsTheWatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	// One update per poll, forever: only the write failure can end this
	// watch.
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &endlessSource{update: backend.Update{
			Event: backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}},
			At:    at,
		}},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	// The consumer left: every write to this writer fails with EPIPE.
	broken := &brokenWriter{err: syscall.EPIPE}

	root := newRootCommand()
	root.SetOut(broken)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	select {
	case err := <-done:
		// The watch ended. Whether the error surfaces as the command's return
		// value is part of the fix under test; what must hold is that the
		// watch ENDED rather than polling forever.
		assert.Error(t, err, "a watch whose output consumer is gone must not report success")
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end after its output consumer went away")
	}
}

// TestMonitorWriteFailureNotOnPlainBuffer is the companion: a healthy writer
// (a bytes.Buffer, like every test and any pipe with a live reader) never
// ends the watch early. The EPIPE path must trigger on write failure only.
func TestMonitorWriteFailureNotOnPlainBuffer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	src := &blockingSource{watched: make(chan backend.Target, 1)}
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:   "relay",
		Kinds:  []backend.Kind{backend.KindPR},
		Source: src,
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	select {
	case <-src.watched:
	case <-time.After(10 * time.Second):
		t.Fatal("watch never started")
	}
	// The watch is alive with a healthy writer; a moment is enough to prove
	// the write path did not cancel it.
	select {
	case err := <-done:
		t.Fatalf("healthy watch ended early: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// endlessSource delivers the same update forever, one per poll tick, and
// closes only when the caller's context ends. It models a target that keeps
// producing changes so only a client-side fault can end the watch.
type endlessSource struct {
	update  backend.Update
	every   time.Duration
	watched chan backend.Target
}

func (s *endlessSource) Watch(ctx context.Context, t backend.Target, _ backend.WatchOptions) (<-chan backend.Update, error) {
	if s.watched != nil {
		select {
		case s.watched <- t:
		default:
		}
	}
	every := s.every
	if every == 0 {
		every = 20 * time.Millisecond
	}
	ch := make(chan backend.Update, 4)
	go func() {
		defer close(ch)
		for {
			u := s.update
			u.Target = t
			select {
			case ch <- u:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(every):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// brokenWriter is a consumer that has left: every write fails with the
// supplied error, the way a closed pipe reports EPIPE to its writer.
type brokenWriter struct{ err error }

func (w *brokenWriter) Write(p []byte) (int, error) { return 0, w.err }

var _ io.Writer = (*brokenWriter)(nil)

// TestMonitorUntilMetBeatsWriteFailure pins the precedence the review
// flagged: when a --until member fires and the write of the triggering event
// fails, the watch reports the condition MET (exit 0, nil), not the write
// failure. The member did fire; a dead consumer must not re-answer it.
func TestMonitorUntilMetBeatsWriteFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	endpoint := serveTestBackend(t, remote.ServerConfig{
		Name:  "relay",
		Kinds: []backend.Kind{backend.KindPR},
		Source: &staticSource{
			updates: []backend.Update{
				{
					Event: backend.Event{Type: backend.EventMerged},
					At:    at,
				},
			},
		},
	})
	t.Setenv(backendEndpointEnv, endpoint)
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()
	apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

	root := newRootCommand()
	root.SetOut(&brokenWriter{err: syscall.EPIPE})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"7", "-R", "o/r", "--until", "merged"})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	select {
	case err := <-done:
		require.NoError(t, err, "the condition fired; a failed write of the triggering event must not change the answer")
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end")
	}
}
