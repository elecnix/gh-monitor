// Package eventlog writes every backend event an operator's watch consumes
// to daily append-only JSONL files (issue #86).
//
// It sits above the backend layer: callers hand it the updates their watch
// loop receives — from the built-in gh backend, the shared daemon, or an
// out-of-process broker sub-daemon — and it records each one as a single
// JSON line. The file is chosen by day (events-YYYY-MM-DD.jsonl), so rotation
// is a filename change, never a rewrite; a new day means a new file. Files
// older than the retention window are pruned when the day rolls over.
//
// The log also carries the daemon's own activity (issue #122): one record per
// hub fetch and one per failover from a sub-daemon to the hub. Those records
// describe the polling itself rather than a change on a target, so they are
// separate kinds — a fetch that found nothing changed delivers no update at
// all, and its absence is exactly what made polling cost invisible.
//
// Logging is a witness, not a dependency: a write failure surfaces as an
// error return so the caller can disable the log and say so, but it must
// never take the watch down.
package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// now is the clock; a package variable so tests pin the day.
var now = time.Now

// fileNamePrefix and ext define the on-disk naming scheme. Pruning only ever
// touches files matching this pattern.
const (
	fileNamePrefix = "events-"
	dayFormat      = "2006-01-02"
	fileExt        = ".jsonl"
)

// FileName returns the log file name for a given day.
func FileName(day time.Time) string {
	return fileNamePrefix + day.Format(dayFormat) + fileExt
}

// Writer appends events to one JSONL file per day inside dir.
type Writer struct {
	dir      string
	keepDays int

	mu     sync.Mutex
	f      *os.File
	day    string
	closed bool
}

// New creates the log directory (0755) if missing, prunes files older than
// keepDays, and returns a ready writer. keepDays below 1 means keep forever.
func New(dir string, keepDays int) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create event log dir: %w", err)
	}
	w := &Writer{dir: dir, keepDays: keepDays}
	if err := w.prune(); err != nil {
		// Pruning is best-effort housekeeping; a failure here (permissions
		// on one stray file, say) must not disable logging.
		fmt.Fprintf(os.Stderr, "gh-monitor: event log prune: %v\n", err)
	}
	return w, nil
}

// Dir returns the directory the writer logs into.
func (w *Writer) Dir() string { return w.dir }

// The record kinds the daemon writes for its own activity (issue #122).
//
// They are separate from the update vocabulary because they describe the
// polling itself, not the target: a fetch that found nothing changed
// delivers no update at all, and a failover is a routing decision rather
// than a change. A log that only records delivered updates cannot show how
// often the hub polls, or which watches fell back to it.
const (
	// KindHubFetch records one hub fetch: the target, the tier it was read
	// at, and whether the read changed anything.
	KindHubFetch = "hub-fetch"
	// KindHubFailover records a watch moving from a sub-daemon to the hub.
	KindHubFailover = "hub-failover"
)

// FetchRecord is one hub fetch (issue #122): the target that was read, the
// tier it was read at, and whether the read changed anything.
//
// The "changed" answer is what a log of delivered updates cannot give. A poll
// that finds nothing changed delivers no update, so a log built only from
// deliveries shows how often the hub found something and nothing about how
// often it looked — which is the number that decides whether the polling is
// worth its API cost.
type FetchRecord struct {
	// Kind names the record: KindHubFetch.
	Kind string `json:"kind"`
	// Target is what was read.
	Target backend.Target `json:"target"`
	// Tier is the query tier the read ran at ("TierFull", "TierStatus", …).
	// It is the record of what the read could see: a shed tier did not read
	// the surfaces it sheds.
	Tier string `json:"tier"`
	// Changed reports whether the read found anything different from the
	// previous observation of this target.
	Changed bool `json:"changed"`
	// At is when the read happened.
	At time.Time `json:"at"`
}

// NewFetchRecord builds the record of one hub fetch. The tier is the query
// tier the read ran at; changed reports whether the read found anything
// different from the previous observation of the target.
//
// The record's kind is set here, so a record cannot be written under the
// wrong kind.
func NewFetchRecord(target backend.Target, tier string, changed bool, at time.Time) FetchRecord {
	return FetchRecord{
		Kind:    KindHubFetch,
		Target:  target,
		Tier:    tier,
		Changed: changed,
		At:      at,
	}
}

// NewFailoverRecord builds the record of one watch that fell back from a
// sub-daemon to the hub. from is the sub-daemon that stopped serving the
// watch, and reason says why the watch moved.
//
// The record's kind is set here, so a record cannot be written under the
// wrong kind.
func NewFailoverRecord(target backend.Target, from, reason string, at time.Time) FailoverRecord {
	return FailoverRecord{
		Kind:   KindHubFailover,
		Target: target,
		From:   from,
		Reason: reason,
		At:     at,
	}
}

// FailoverRecord is one watch that fell back from a sub-daemon to the hub
// (issue #122).
//
// It is recorded because the fallback is not free: a sub-daemon that serves a
// watch through a webhook costs no API budget, and the hub that takes over
// spends one. A log that shows only what was delivered cannot say which
// watches gave up that saving, or how often.
type FailoverRecord struct {
	// Kind names the record: backend.SourceHubFailover.
	Kind string `json:"kind"`
	// Target is the watch that moved.
	Target backend.Target `json:"target"`
	// From is the sub-daemon that stopped serving the watch.
	From string `json:"from"`
	// Reason says why the watch moved.
	Reason string `json:"reason"`
	// At is when the watch moved.
	At time.Time `json:"at"`
}

// Log appends one event as a single JSON line. The event is marshalled as-is
// — callers pass their update envelope so the log records what the backend
// delivered, not what the renderer made of it.
func (w *Writer) Log(event any) error {
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("event log is closed")
	}
	day := now().Format(dayFormat)
	if w.f == nil || w.day != day {
		if err := w.rotateLocked(day); err != nil {
			return err
		}
	}
	// One Write per line under the lock: O_APPEND makes each write atomic
	// with respect to other appends, so concurrent writers interleave whole
	// lines rather than tearing them.
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// Close releases the current day's file. The writer is unusable afterwards.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotateLocked closes the previous day's file, opens today's for appending,
// and prunes past the retention window. Caller holds w.mu.
func (w *Writer) rotateLocked(day string) error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	f, err := os.OpenFile(filepath.Join(w.dir, FileName(now())), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	w.f = f
	w.day = day
	if err := w.prune(); err != nil {
		fmt.Fprintf(os.Stderr, "gh-monitor: event log prune: %v\n", err)
	}
	return nil
}

// prune removes events-*.jsonl files older than keepDays. Caller holds no
// lock requirements; prune only runs under w.mu via rotateLocked/New.
func (w *Writer) prune() error {
	if w.keepDays < 1 {
		return nil // keep forever
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return fmt.Errorf("read log dir: %w", err)
	}
	cutoff := now().AddDate(0, 0, -(w.keepDays - 1)) // keep exactly keepDays: today and the keepDays-1 days before it
	var stale []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, fileNamePrefix) || !strings.HasSuffix(name, fileExt) {
			continue
		}
		day, err := time.Parse(dayFormat, strings.TrimSuffix(strings.TrimPrefix(name, fileNamePrefix), fileExt))
		if err != nil {
			continue // not one of ours; leave it alone
		}
		if day.Before(cutoff) {
			stale = append(stale, filepath.Join(w.dir, name))
		}
	}
	sort.Strings(stale)
	for _, path := range stale {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}
