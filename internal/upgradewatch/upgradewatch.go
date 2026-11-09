// Package upgradewatch decides when the binary a resident process was launched
// from has been replaced on disk by an upgrade (issue #216).
//
// The daemon already watches the installed binary (issue #73/#74). A resident
// *watch client* — the process that polls, renders notifications and prints the
// first-poll ack — did not, so an upgrade landed on disk while the watch kept
// executing the image it started with, and nothing short of restarting that
// watch replaced it.
//
// Why these signals, in this order of cost:
//
//   - Size, mtime, inode and device come from one stat: the only probe cheap
//     enough to run forever on a process that must stay invisible. `gh
//     extension upgrade` rewrites the installed file in place, so the inode
//     usually survives and the size often does too; mtime is what moves. The
//     inode is kept because it distinguishes an in-place rewrite from a
//     rename-over, and because a stat-equality test that ignores identity can
//     compare two different files.
//   - SHA-256 is authoritative about content, so it is what actually decides.
//     It is computed once at start and thereafter only for a candidate whose
//     metadata already changed — never on every tick. Reading 13 MB every 15
//     seconds for a process that is supposed to be invisible is not acceptable;
//     reading it twice per upgrade is free.
//   - A candidate must be observed twice before it is acted on: `gh extension
//     upgrade` truncates and rewrites the installed file, and exec'ing the
//     half-written result would fail at best. The settle window also collapses
//     a burst of writes into one decision.
//
// Loop prevention is structural rather than a counter. The replacement process
// builds a new Tracker whose baseline is the binary it is already running, so
// it can only ever decide to replace itself with a *different* file; an
// unchanged binary is a stat-equality no-op. MinGap is a second, independent
// guard for the pathological case where something rewrites the installed file
// continuously: the watch keeps serving rather than thrashing.
package upgradewatch

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"time"
)

// DefaultInterval is how often a resident watch re-checks the installed
// binary. One stat per tick; the tick is deliberately slow because an upgrade
// landing a minute or two late costs nothing, while a busy loop over the
// installed file on every watch of every session costs something.
const DefaultInterval = 2 * time.Minute

// DefaultMinGap is the floor between two replacements performed by one
// process. A successor builds its own Tracker, so this only guards against a
// file that keeps changing under a single process.
const DefaultMinGap = 30 * time.Second

// Identity is one observation of a file on disk.
type Identity struct {
	Path    string
	Size    int64
	ModTime time.Time
	Dev     uint64
	Ino     uint64

	sum     [sha256.Size]byte
	haveSum bool
}

// Stat observes path's metadata. The content hash is left uncomputed.
func Stat(path string) (Identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Identity{}, err
	}
	dev, ino := fileIdentity(info)
	return Identity{
		Path:    path,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Dev:     dev,
		Ino:     ino,
	}, nil
}

// SameFile reports whether two observations describe the same file version.
// Content is not consulted: this is the cheap pre-test.
func (i Identity) SameFile(o Identity) bool {
	return i.Size == o.Size &&
		i.ModTime.Equal(o.ModTime) &&
		i.Dev == o.Dev &&
		i.Ino == o.Ino
}

// Sum returns the file's SHA-256, reading it at most once per observation.
// A read failure is returned rather than cached, so a transient error cannot
// masquerade as "same content".
func (i *Identity) Sum() ([sha256.Size]byte, error) {
	if i.haveSum {
		return i.sum, nil
	}
	f, err := os.Open(i.Path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [sha256.Size]byte{}, err
	}
	copy(i.sum[:], h.Sum(nil))
	i.haveSum = true
	return i.sum, nil
}

// Tracker remembers the binary a process is running and decides when a
// different one has settled on disk.
type Tracker struct {
	cur     Identity
	pending *Identity
	lastAct time.Time

	// MinGap is the minimum time between two replacements from one Tracker.
	// Zero means DefaultMinGap.
	MinGap time.Duration
}

// New snapshots path as the binary currently in use.
func New(path string) (*Tracker, error) {
	cur, err := Stat(path)
	if err != nil {
		return nil, err
	}
	if _, err := cur.Sum(); err != nil {
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}
	return &Tracker{cur: cur, MinGap: DefaultMinGap}, nil
}

// Poll reports whether the file now holds a different binary than the one in
// use, and adopts it when it does. It returns false while a change is still
// unconfirmed (seen once) and for metadata-only churn such as `touch`.
//
// An error means the file could not be inspected; the caller should log it
// once and keep serving — a missing or unreadable installed binary is not a
// reason to drop a watch.
func (t *Tracker) Poll() (bool, error) {
	now, err := Stat(t.cur.Path)
	if err != nil {
		return false, err
	}
	if now.SameFile(t.cur) {
		t.pending = nil
		return false, nil
	}
	if t.pending == nil || !now.SameFile(*t.pending) {
		// First sighting, or the file moved again — wait for the next tick to
		// confirm the writer has finished.
		t.pending = &now
		return false, nil
	}

	// Settled. Content decides.
	nowSum, err := now.Sum()
	if err != nil {
		// Treat as unstable rather than changed: keep serving until the file
		// can be read and hashed.
		t.pending = &now
		return false, err
	}
	curSum, err := t.cur.Sum()
	if err != nil {
		return false, err
	}
	if nowSum == curSum {
		// Same binary: metadata churn only. Adopt the new metadata so the
		// next tick does not re-examine it.
		t.cur = now
		t.pending = nil
		return false, nil
	}

	gap := t.MinGap
	if gap == 0 {
		gap = DefaultMinGap
	}
	if !t.lastAct.IsZero() && time.Since(t.lastAct) < gap {
		// Something is rewriting the installed binary continuously. Keep
		// serving instead of replacing the watch again.
		return false, nil
	}

	t.cur = now
	t.pending = nil
	t.lastAct = time.Now()
	return true, nil
}

// Current returns the binary in use as last adopted.
func (t *Tracker) Current() Identity { return t.cur }
