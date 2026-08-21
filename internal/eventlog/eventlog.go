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
