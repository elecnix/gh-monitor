// Package reexec lets a resident process run from a copy of the binary that
// `gh extension upgrade` is free to replace in place (issue #73).
//
// `gh extension upgrade` writes the downloaded asset into the installed file
// with an in-place truncate+write. Linux refuses that (ETXTBSY, "text file
// busy") while any process has the file exec'd and mapped, and gh-monitor's
// whole job is to stay resident — so an upgrade silently fails whenever a
// watcher runs, which is the normal state.
//
// The fix on this side: before a long-lived command starts its watch, copy
// the binary to a per-user runtime path and exec the copy. The installed file
// is only ever exec'd for the instant of launch; the resident image maps the
// runtime copy's inode, and a rename over the runtime path leaves the running
// image untouched. The installed file is then writable in place, and the
// upgrade lands.
package reexec

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OptOutEnv disables the relaunch-into-runtime-copy behaviour when set to "0".
// Every other value, including unset, keeps it on.
const OptOutEnv = "GH_MONITOR_REEXEC"

// InstalledBinEnv names the file the process was originally launched from.
// The launcher sets it before exec'ing the runtime copy, so a resident daemon
// can watch the installed binary for upgrades and hand off to it (issue #73).
const InstalledBinEnv = "GH_MONITOR_INSTALLED_BIN"

// ReexecDepthEnv counts how many hops this process has already taken. Without
// it the chain has no bound of its own: the only thing that ended it was the
// identity check, and when that assumption breaks — the copy landing on a file
// the next generation does not recognise as its own — every generation copies
// again. That is how a test host filled /tmp with hundreds of gigabytes
// (issue #196).
const ReexecDepthEnv = "GH_MONITOR_REEXEC_DEPTH"

const (
	// maxReexecDepth is a fuse, not a budget. One hop is the whole design: the
	// installed binary exec's the runtime copy and stops there. A second hop
	// tolerates the copy being replaced underneath a running process (a runtime
	// directory reclaimed between the copy and the exec). Anything past that is
	// a loop, and the way to end a loop is to stay where you are and say so.
	maxReexecDepth = 2

	// staleCopyAge is how long an interrupted copy may sit in the runtime
	// directory before it is treated as debris. A successful copy takes
	// milliseconds, so this only ever reclaims files from a generation that died
	// — killed on a helper's own timeout, or by the OOM killer — mid-write.
	staleCopyAge = 5 * time.Minute

	// maxPendingCopies and maxPendingCopyFactor bound how much debris may
	// accumulate before the next attempt refuses to add to it. The threshold is
	// deliberately loose: several gh-monitor processes starting together is
	// ordinary and each leaves a copy mid-write for a few milliseconds, so a
	// tight bound would refuse them and put the installed binary back on the
	// mapped list, which is the problem this package exists to solve. A runaway
	// produces hundreds of copies within the same window, so a loose bound still
	// trips in seconds and caps the damage at a few hundred megabytes.
	maxPendingCopies     = 8
	maxPendingCopyFactor = 8
)

// ChildEnv is the environment for a process this one is about to launch: our
// own, minus the re-exec depth.
//
// A successor daemon is a new launch, not another hop. It starts from the
// installed binary and takes exactly one hop of its own, so it must not inherit
// the count of the chain that spawned it. Inheriting it would be quietly fatal:
// every handoff would add a hop to a chain that only ever re-execs once, and
// after a couple of upgrades the fuse would trip on a perfectly healthy daemon,
// leaving it running from the installed binary — the one state this package
// exists to avoid, since that image cannot be replaced in place.
func ChildEnv() []string {
	prefix := ReexecDepthEnv + "="
	env := os.Environ()
	kept := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			kept = append(kept, kv)
		}
	}
	return kept
}

// reexecToFn is the seam tests use to observe a hop without replacing the test
// process. Production always calls reexecTo.
var reexecToFn = reexecTo

// RuntimePath returns the path the resident copy of the binary lives at:
// $XDG_RUNTIME_DIR/gh-monitor/gh-monitor, falling back to the user cache dir
// (the same precedence the daemon socket uses). The directory need not exist.
func RuntimePath() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "gh-monitor", "gh-monitor"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve runtime copy location: %w", err)
	}
	return filepath.Join(cache, "gh-monitor", "gh-monitor"), nil
}

// MaybeReexec copies the running executable to RuntimePath and, on success,
// replaces this process with the copy (same argv, same environment). It
// returns nil — without re-executing — when the relaunch is opted out, when
// the running executable already is the runtime copy, or on platforms where
// the mechanism is not implemented; those cases simply run in place, exactly
// as before this package existed. A returned error means the copy could not
// be made, or that the chain has already gone further than a working identity
// check ever lets it; callers should log it and run in place rather than fail.
func MaybeReexec() error {
	if os.Getenv(OptOutEnv) == "0" {
		return nil
	}
	if !supported() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	rt, err := RuntimePath()
	if err != nil {
		return err
	}
	if sameFile(exe, rt) {
		return nil
	}
	depth := reexecDepth()
	if depth >= maxReexecDepth {
		return fmt.Errorf("refusing to re-exec again (%s=%d, limit %d): the runtime copy is not being recognised as this process's own, so another hop would only repeat itself",
			ReexecDepthEnv, depth, maxReexecDepth)
	}
	if err := copyExecutable(exe, rt); err != nil {
		return err
	}
	// Remember where we came from: the resident daemon watches that file for
	// upgrades and hands off to it.
	_ = os.Setenv(InstalledBinEnv, exe)
	_ = os.Setenv(ReexecDepthEnv, strconv.Itoa(depth+1))
	return reexecToFn(rt)
}

// reexecDepth reads how many hops this process has taken. A value that cannot
// be read is treated as zero: an inherited value we do not understand is not a
// reason to refuse to start, and the bound still applies once we write the next
// one ourselves.
func reexecDepth() int {
	raw := strings.TrimSpace(os.Getenv(ReexecDepthEnv))
	if raw == "" {
		return 0
	}
	depth, err := strconv.Atoi(raw)
	if err != nil || depth < 0 {
		return 0
	}
	return depth
}

// sameFile reports whether path and the runtime copy are the same file. A
// missing runtime copy is simply "not the same".
func sameFile(path, rt string) bool {
	a, err := os.Stat(path)
	if err != nil {
		return false
	}
	b, err := os.Stat(rt)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// copyExecutable writes src to dst atomically: a temp file in the same
// directory, fsynced, then renamed over dst. The rename is what keeps a
// running image on the old inode safe while the new binary lands.
//
// Before writing anything it reclaims copies abandoned by a previous attempt
// and, if the directory is still unhealthy, refuses to add to it (issue #196).
func copyExecutable(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	removeAbandonedCopies(dir, filepath.Base(dst))
	if err := guardRuntimeDir(src, dir, filepath.Base(dst)); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp copy: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("copy executable: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod runtime copy: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync runtime copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close runtime copy: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install runtime copy: %w", err)
	}
	return nil
}

// removeAbandonedCopies deletes temp copies older than staleCopyAge. A live
// copy is left alone: it may belong to another process mid-hop, and deleting it
// would fail that hop for no reason. Failures to remove are ignored — a file we
// cannot delete is not a reason to refuse to upgrade, and guardRuntimeDir is the
// backstop if the directory stays unhealthy.
func removeAbandonedCopies(dir, base string) {
	matches, err := filepath.Glob(filepath.Join(dir, base+".tmp-*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleCopyAge)
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}

// guardRuntimeDir refuses a copy when the runtime directory already holds more
// interrupted copies than a healthy directory ever would. This is the case that
// turned a looping re-exec into a full root filesystem: each generation added a
// copy of the binary, nothing removed them, and the only symptom was unrelated
// processes failing to write.
func guardRuntimeDir(src, dir, base string) error {
	matches, err := filepath.Glob(filepath.Join(dir, base+".tmp-*"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	var pending int64
	for _, path := range matches {
		if info, err := os.Stat(path); err == nil {
			pending += info.Size()
		}
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil // the copy itself will report this, with a better message
	}
	budget := int64(maxPendingCopyFactor) * srcInfo.Size()
	if len(matches) <= maxPendingCopies && pending <= budget {
		return nil
	}
	return fmt.Errorf("runtime dir %s holds %d interrupted copies (%d bytes, more than %d copies and %d bytes): refusing to add another and fill the disk — remove them and restart",
		dir, len(matches), pending, maxPendingCopies, budget)
}
