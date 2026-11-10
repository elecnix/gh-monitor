package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/elecnix/gh-monitor/internal/broker"
	"github.com/elecnix/gh-monitor/internal/handoff"
	"github.com/elecnix/gh-monitor/internal/transportlog"
)

// transportLogPath is where the daemon records its broker connection
// (issue #223): $GH_MONITOR_TRANSPORT_LOG, else transport.jsonl in the user
// cache directory, next to the coverage map and the event log.
func transportLogPath() string {
	if p := os.Getenv("GH_MONITOR_TRANSPORT_LOG"); p != "" {
		return p
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "gh-monitor", "transport.jsonl")
}

// openTransportLog opens the daemon's transport log, creating its directory.
// A log that cannot be opened is reported once and the transport runs
// without it: the record is a witness, never a dependency.
func openTransportLog(cmd *cobra.Command) *transportlog.Writer {
	path := transportLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: transport log disabled (%v)\n", err)
		return nil
	}
	w, err := transportlog.Open(path, transportlog.DefaultMaxBytes)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "gh-monitor daemon: transport log disabled (%v)\n", err)
		return nil
	}
	return w
}

// noticeBroadcaster is the part of the hub the transport observer needs.
type noticeBroadcaster interface {
	BroadcastNotice(string)
}

// transportObserver returns the watcher's OnTransition hook: every
// transition is written to the log, and the two episode notices are sent
// to every watching client through the hub. The watcher serializes the
// calls, so the one-failure latch needs no lock.
//
// The loss of a connection already reaches the clients through
// SetBrokerHealth. The outage notice is the second, distinct one: it says
// the transport has stayed down past the threshold and the watch is served
// by polling until further notice. The recovered notice closes it.
func transportObserver(h noticeBroadcaster, w *transportlog.Writer, stderr io.Writer) func(broker.Transition) {
	failed := false
	return func(t broker.Transition) {
		if w != nil && !failed {
			if err := w.Log(transportlog.FromTransition(t)); err != nil {
				failed = true
				_, _ = fmt.Fprintf(stderr, "gh-monitor daemon: transport log disabled (%v)\n", err)
			}
		}
		switch t.Kind {
		case broker.TransitionOutage:
			h.BroadcastNotice(outageNotice(t))
		case broker.TransitionRecovered:
			h.BroadcastNotice(recoveredNotice(t))
		}
	}
}

// outageNotice is the text every watch receives once the transport has
// been down past the threshold. It says how long, how many attempts
// failed, and what that means for the watch: a comment or review posted
// while the transport is down arrives at the next poll, not as it happens.
func outageNotice(t broker.Transition) string {
	return fmt.Sprintf(
		"⚠️ broker transport down for %s after %d failed reconnect attempts (since %s): this watch is falling back to polling; a comment or review posted while it is down arrives at the next poll, not as it happens",
		t.At.Sub(t.Since).Round(time.Second), t.Attempt, t.Since.UTC().Format(time.RFC3339))
}

// recoveredNotice closes the outage: the wake path is back, and anything
// posted during the outage was observed by polling only.
func recoveredNotice(t broker.Transition) string {
	return fmt.Sprintf(
		"✅ broker transport recovered after %s down (%d attempts, since %s): wake events resume; anything posted during the outage was observed by polling only",
		t.At.Sub(t.Since).Round(time.Second), t.Attempt, t.Since.UTC().Format(time.RFC3339))
}

// doctorNow is doctor's clock for the transport ages. A variable so a test
// can pin it.
var doctorNow = time.Now

// printTransport adds the broker transport's state to doctor's report, read
// from the daemon's transport log. A daemon with no broker configured writes
// no log, and then doctor prints nothing about it. A log older than the
// daemon describes an earlier daemon's connection, and doctor says so.
func printTransport(out io.Writer, path string, info handoff.DaemonInfo, infoErr error, now time.Time) {
	s, err := transportlog.Summarize(path)
	if err != nil {
		if !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(out, "broker transport: cannot read %s (%v)\n", path, err)
		}
		return
	}
	if s.State == "" {
		_, _ = fmt.Fprintf(out, "broker transport: the log at %s has no records\n", path)
		return
	}
	// The first record of a daemon's connection is written as the daemon
	// starts serving, so a minute's tolerance separates "written by this
	// daemon" from "left by an earlier one".
	if infoErr == nil && !info.StartedAt.IsZero() && s.LastRecord.Before(info.StartedAt.Add(-time.Minute)) {
		_, _ = fmt.Fprintf(out, "broker transport: the log at %s predates this daemon (last record %s), so it describes an earlier daemon's connection\n",
			path, s.LastRecord.Format(time.RFC3339))
	}
	for _, line := range s.Describe(now) {
		_, _ = fmt.Fprintf(out, "broker transport: %s\n", line)
	}
}
