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
	"crypto/sha256"
	"encoding/hex"
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

	// copyRetentionAge is how long an unused content-addressed copy is kept.
	// Copies are one per build actually launched, not one per launch, but a
	// rebuild on every `make build` would still leave a 13 MB file per build in
	// a tmpfs, so old ones are reclaimed. Unlinking a copy another process is
	// running from is safe: the running image keeps the inode alive.
	copyRetentionAge = 24 * time.Hour
)

// copyPattern is the name a runtime copy is stored under. The key in it makes
// the path a function of the binary's identity, never of the time it was
// written (see CopyPathFor).
const copyPattern = "gh-monitor-"

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

// RuntimeDir returns the directory runtime copies live in:
// $XDG_RUNTIME_DIR/gh-monitor, falling back to the user cache dir (the same
// precedence the daemon socket uses). The directory need not exist.
func RuntimeDir() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "gh-monitor"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve runtime copy location: %w", err)
	}
	return filepath.Join(cache, "gh-monitor"), nil
}

// RuntimePath returns the path of the runtime copy for the running executable
// — the path a resident process runs from.
func RuntimePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	return CopyPathFor(exe)
}

// CopyPathFor returns the runtime copy path for the binary at exe.
//
// The name is derived from the file's identity — device, inode, size and
// modification time — not from its path. That is what makes launching two
// different builds concurrently safe: a single shared "current copy" path is
// rewritten by every launch, so a process can exec a copy of a binary other
// than its own between its rename and its exec. Content-addressed copies are
// written once and never rewritten, so the window is gone rather than
// narrowed. The depth fuse below still stands behind this, because an identity
// check is exactly the assumption that failed in issue #196.
func CopyPathFor(exe string) (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	key, err := identityKey(exe)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, copyPattern+key), nil
}

// identityKey is a short, stable fingerprint of one binary file. It must
// change whenever the file's contents do, and must not require reading 13 MB
// on every launch: size, mtime (nanosecond resolution), device and inode are
// enough for that, and the inode changes on any atomic replace.
func identityKey(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	inode, device := identityFields(fi)
	h := sha256.New()
	// A hash write cannot fail, but the check is what tells every linter —
	// and the next reader — that nothing is being ignored here.
	if _, err := fmt.Fprintf(h, "%d:%d:%d:%d", fi.Size(), fi.ModTime().UnixNano(), inode, device); err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// isRuntimeCopy reports whether path is a runtime copy: a file directly in the
// runtime directory under the copy-name pattern. Asking the file *where it
// lives*, never only what it contains, is what stops a copy being copied again
// forever — a copy's identity differs from the binary it was made from.
func isRuntimeCopy(path string) bool {
	dir, err := RuntimeDir()
	if err != nil {
		return false
	}
	return filepath.Dir(path) == dir && strings.HasPrefix(filepath.Base(path), copyPattern)
}

// pruneCopies removes runtime copies that are not keep, once they have gone
// unused for copyRetentionAge. A copy another process is running from can be
// unlinked safely: the running image keeps the inode alive and nothing re-reads
// the path.
func pruneCopies(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-copyRetentionAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), copyPattern) || strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if full == keep {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(full)
	}
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
	rt, err := CopyPathFor(exe)
	if err != nil {
		return err
	}
	// Already a runtime copy — this is the re-entered process, or a resident
	// worker started from one. Either test settling it is enough: the path says
	// where the file lives, the identity says whether it is the copy we would
	// have made.
	if isRuntimeCopy(exe) || sameFile(exe, rt) {
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
	pruneCopies(filepath.Dir(rt), rt)
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
