// Package transportlog is the daemon's record of its broker connection
// (issue #223): one JSON line per connect attempt, per state change, and
// per episode notice, in a file the daemon owns next to its other state.
//
// A daemon started detached has /dev/null for stdout and stderr, so the
// reconnect loop's attempts, delays and errors had nowhere to go. A reader
// of this file can answer when the transport last worked, when it stopped,
// what the error said, and whether the daemon is still retrying, without
// a process listing or lsof. `gh monitor doctor` reads it for that.
//
// The file is bounded. A write that would pass the size cap first renames
// the file to its rotated name, replacing the previous rotated file, so a
// flapping transport keeps at most two files of the cap's size.
//
// A record carries the endpoint host, the error class, and the redacted
// error message. It never carries the connection URL, a header, a token or
// an event payload: the record is built from a broker.Transition, which has
// none of those, and the message goes through broker.Redact.
package transportlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/elecnix/gh-monitor/internal/broker"
)

// DefaultMaxBytes is the size cap of the current file. A record is under
// 300 bytes, so the cap keeps the last few thousand transitions.
const DefaultMaxBytes = 1 << 20

// Record is one line of the log.
type Record struct {
	// At is when the transition happened.
	At time.Time `json:"at"`
	// Event is the transition kind: connecting, healthy, degraded, outage,
	// or recovered.
	Event string `json:"event"`
	// Host is the endpoint's host name.
	Host string `json:"host,omitempty"`
	// Attempt is the connect attempt this record is about, counted from
	// the first failure of the episode.
	Attempt int `json:"attempt,omitempty"`
	// ErrorClass is broker.Classify's name for the error. Degraded only.
	ErrorClass string `json:"error_class,omitempty"`
	// Error is the redacted error message. Degraded only.
	Error string `json:"error,omitempty"`
	// RetryIn is the delay before the next attempt, as a Go duration.
	// Degraded only.
	RetryIn string `json:"retry_in,omitempty"`
	// Since is when the episode began, in RFC 3339 with the full fraction,
	// so an age computed from it is exact. Degraded, outage and recovered
	// only.
	Since string `json:"since,omitempty"`
}

// FromTransition builds the record of one transition. The error message is
// redacted here, so no caller can write an unredacted one.
func FromTransition(t broker.Transition) Record {
	r := Record{
		At:      t.At,
		Event:   string(t.Kind),
		Host:    t.Host,
		Attempt: t.Attempt,
	}
	if t.Err != nil {
		r.ErrorClass = broker.Classify(t.Err)
		r.Error = broker.Redact(t.Err.Error())
	}
	if t.RetryIn > 0 {
		r.RetryIn = t.RetryIn.String()
	}
	if !t.Since.IsZero() {
		r.Since = t.Since.UTC().Format(time.RFC3339Nano)
	}
	return r
}

// RotatedPath is where the previous generation of the file at path is kept.
func RotatedPath(path string) string { return path + ".1" }

// Writer appends records to one file and rotates it at a size cap.
type Writer struct {
	path     string
	maxBytes int64

	mu     sync.Mutex
	f      *os.File
	size   int64
	closed bool
}

// Open opens the file at path for appending, creating it if needed, and
// returns a writer that rotates it once it would pass maxBytes. A maxBytes
// of zero or less means DefaultMaxBytes.
func Open(path string, maxBytes int64) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	w := &Writer{path: path, maxBytes: maxBytes}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Path returns the file the writer appends to.
func (w *Writer) Path() string { return w.path }

func (w *Writer) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open transport log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat transport log: %w", err)
	}
	w.f = f
	w.size = st.Size()
	return nil
}

// Log appends one record as a single JSON line, rotating first when the
// line would take the file past the cap.
func (w *Writer) Log(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal transport record: %w", err)
	}
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("transport log is closed")
	}
	if w.size > 0 && w.size+int64(len(line)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := w.f.Write(line)
	w.size += int64(n)
	if err != nil {
		return fmt.Errorf("append transport record: %w", err)
	}
	return nil
}

// rotateLocked moves the current file to the rotated path, replacing the
// previous rotated file, and opens a new current file.
func (w *Writer) rotateLocked() error {
	_ = w.f.Close()
	w.f = nil
	if err := os.Rename(w.path, RotatedPath(w.path)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate transport log: %w", err)
	}
	return w.openLocked()
}

// Close releases the file. The writer is unusable afterwards.
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

// Summary is the transport's state as the log last recorded it.
type Summary struct {
	// Host is the endpoint's host name.
	Host string
	// State is the last recorded connection state: connecting, healthy, or
	// degraded.
	State string
	// Since is when that state was entered.
	Since time.Time
	// Attempt is the connect attempt the last record was about.
	Attempt int
	// LastHealthy is when the transport last connected, zero if never.
	LastHealthy time.Time
	// LastError is the last recorded error, as "class: message".
	LastError string
	// LastErrorAt is when it was recorded.
	LastErrorAt time.Time
	// Retrying reports that the daemon is between attempts or in one.
	Retrying bool
	// RetryIn is the delay the last degraded record announced.
	RetryIn time.Duration
	// DownSince is when the current episode began, zero while healthy.
	DownSince time.Time
	// OutageOpen reports that the watches were told of an outage that no
	// recovered record has closed.
	OutageOpen bool
	// LastRecord is the newest record's timestamp.
	LastRecord time.Time
}

// Summarize reads the log at path, and the rotated file before it, and
// returns the state its newest records describe. A torn or unreadable line
// is skipped. A missing current file is reported with os.ErrNotExist.
func Summarize(path string) (Summary, error) {
	var s Summary
	if _, err := os.Stat(path); err != nil {
		return s, err
	}
	if err := s.readFile(RotatedPath(path)); err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if err := s.readFile(path); err != nil {
		return s, err
	}
	return s, nil
}

func (s *Summary) readFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		s.apply(r)
	}
	return sc.Err()
}

// apply folds one record into the summary, in file order.
func (s *Summary) apply(r Record) {
	if r.Host != "" {
		s.Host = r.Host
	}
	s.LastRecord = r.At
	since, _ := time.Parse(time.RFC3339, r.Since)
	switch broker.TransitionKind(r.Event) {
	case broker.TransitionConnecting:
		s.State = r.Event
		s.Since = r.At
		s.Attempt = r.Attempt
		s.Retrying = true
	case broker.TransitionHealthy:
		s.State = r.Event
		s.Since = r.At
		s.Attempt = 0
		s.LastHealthy = r.At
		s.Retrying = false
		s.RetryIn = 0
		s.DownSince = time.Time{}
	case broker.TransitionDegraded:
		s.State = r.Event
		s.Since = r.At
		s.Attempt = r.Attempt
		s.Retrying = true
		s.RetryIn, _ = time.ParseDuration(r.RetryIn)
		s.LastError = strings.TrimPrefix(r.ErrorClass+": "+r.Error, ": ")
		s.LastErrorAt = r.At
		s.DownSince = since
	case broker.TransitionOutage:
		s.OutageOpen = true
		s.DownSince = since
	case broker.TransitionRecovered:
		s.OutageOpen = false
		s.DownSince = time.Time{}
	}
}

// Describe renders the summary as the lines doctor prints, each without
// its "broker transport:" prefix. now is the reader's clock, for the ages.
func (s Summary) Describe(now time.Time) []string {
	age := func(t time.Time) string { return now.Sub(t).Round(time.Second).String() }
	var lines []string
	first := fmt.Sprintf("%s for %s", s.State, age(s.Since))
	if s.Host != "" {
		first += " (host " + s.Host
		if s.Attempt > 0 {
			first += fmt.Sprintf(", attempt %d", s.Attempt)
		}
		first += ")"
	}
	if s.Retrying && s.RetryIn > 0 && s.State == string(broker.TransitionDegraded) {
		first += ", retrying in " + s.RetryIn.String()
	}
	lines = append(lines, first)
	switch {
	case s.LastHealthy.IsZero():
		lines = append(lines, "last healthy: never")
	case s.State != string(broker.TransitionHealthy):
		lines = append(lines, fmt.Sprintf("last healthy %s ago (%s)", age(s.LastHealthy), s.LastHealthy.Format(time.RFC3339)))
	}
	if s.LastError != "" {
		lines = append(lines, fmt.Sprintf("last error %s ago: %s", age(s.LastErrorAt), s.LastError))
	}
	if !s.DownSince.IsZero() {
		line := fmt.Sprintf("down for %s", age(s.DownSince))
		if s.OutageOpen {
			line += "; the watches were told and are served by polling"
		}
		lines = append(lines, line)
	}
	if s.LastRecord.Before(now.Add(-24 * time.Hour)) {
		lines = append(lines, fmt.Sprintf("last record %s ago", age(s.LastRecord)))
	}
	return lines
}
