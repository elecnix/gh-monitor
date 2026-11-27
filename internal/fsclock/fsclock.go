// Package fsclock lets a test wait for a filesystem's timestamp clock to tick.
//
// A filesystem does not stamp a write with the exact time. Linux before 6.13
// stamps mtime and ctime from a coarse clock that moves once per jiffy, about
// 1 ms to 4 ms; ext4 on 6.11 was measured at 1 ms steps (issue #237). Two
// writes inside the same tick get the same mtime and the same ctime. A test
// that writes a file, rewrites it at once, and expects the rewrite to change
// a timestamp therefore fails most of the time on such a kernel and passes on
// a newer one.
//
// WaitForTimestampTick waits until the clock has moved past the file's
// current timestamps, so the next write is stamped later. It lives in its own
// package so several packages' test suites can share it, the same reason
// internal/testrec exists.
package fsclock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pollStep is how long WaitForTimestampTick sleeps between probes. It is well
// under the coarsest known tick, so the wait ends soon after the tick does.
const pollStep = 200 * time.Microsecond

// maxWait bounds the wait. A tick is a few milliseconds at most, so reaching
// this means the filesystem's clock does not move, and the test cannot work.
const maxWait = 5 * time.Second

// WaitForTimestampTick returns once a write to any file in path's directory
// would get a timestamp later than the one path has now: its ctime, or its
// mtime on a platform whose stat this package does not read a ctime from.
//
// It does not sleep a fixed time. It writes a probe file next to path, on
// the same filesystem and so on the same clock, and stats it every pollStep
// until the probe's stamp is later than path's. The probe is removed before
// it returns, and path itself is never touched.
func WaitForTimestampTick(tb testing.TB, path string) {
	tb.Helper()
	info, err := os.Stat(path)
	if err != nil {
		tb.Fatalf("fsclock: stat %s: %v", path, err)
	}
	mark := stamp(info)

	probe, err := os.CreateTemp(filepath.Dir(path), ".fsclock-probe-*")
	if err != nil {
		tb.Fatalf("fsclock: create probe next to %s: %v", path, err)
	}
	probePath := probe.Name()
	defer func() { _ = os.Remove(probePath) }()
	if err := probe.Close(); err != nil {
		tb.Fatalf("fsclock: close probe %s: %v", probePath, err)
	}

	deadline := time.Now().Add(maxWait)
	for {
		// A write stamps the probe's mtime and ctime with the filesystem's
		// current time, the same stamp a write to path would get.
		if err := os.WriteFile(probePath, []byte("tick"), 0o600); err != nil {
			tb.Fatalf("fsclock: write probe %s: %v", probePath, err)
		}
		probeInfo, err := os.Stat(probePath)
		if err != nil {
			tb.Fatalf("fsclock: stat probe %s: %v", probePath, err)
		}
		if stamp(probeInfo).After(mark) {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("fsclock: the timestamp clock of %s did not move past %v in %v", filepath.Dir(path), mark, maxWait)
		}
		time.Sleep(pollStep)
	}
}

// stamp returns the timestamp a write moves and nothing else can set back:
// the inode change time. A test may set mtime anywhere, even into the
// future, so mtime is the fallback only where no ctime is read.
func stamp(info os.FileInfo) time.Time {
	if ctime := changeTime(info); !ctime.IsZero() {
		return ctime
	}
	return info.ModTime()
}
