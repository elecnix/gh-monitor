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

// copyPattern is the name a runtime copy is stored under. The key in it makes
// the path a function of the binary's identity, never of the time it was
// written (see CopyPathFor).
const copyPattern = "gh-monitor-"

// staleCopyAge is how old an unused runtime copy must be before it is pruned.
// Copies are content-addressed and small in number — one per build actually
// launched — but a rebuild on every `make build` would otherwise leave one
// 13 MB file per build behind in a tmpfs.
const staleCopyAge = 24 * time.Hour

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

// RuntimePath returns the path of the runtime copy for the running
// executable — the path a resident process runs from.
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
// different builds concurrently safe: a shared "current copy" path was
// rewritten by every launch, so a process could exec a copy of a binary
// other than its own between its rename and its exec (and did: a two-day-old
// watcher reported features from a build two weeks older than its own).
// Content-addressed copies are written once and never rewritten, so the
// window is gone rather than narrowed.
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

// MaybeReexec copies the running executable to RuntimePath and, on success,
// replaces this process with the copy (same argv, same environment). It
// returns nil — without re-executing — when the relaunch is opted out, when
// the running executable already is the runtime copy, or on platforms where
// the mechanism is not implemented; those cases simply run in place, exactly
// as before this package existed. A returned error means the copy could not
// be made; callers should log it and run in place rather than fail.
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
	// Already the runtime copy, either at the path this identity maps to or
	// at another copy path (a rebuild's copy, a hard-link in a test). Asking
	// *where the file lives*, never only what it contains, is what stops a
	// copy being copied again forever: a copy's identity differs from the
	// binary it was made from.
	if isRuntimeCopy(exe) || sameFile(exe, rt) {
		return nil
	}
	// An existing copy for this identity is by construction the same file, so
	// there is nothing to write. This is the common path: every watch of a
	// long-lived session relaunches into the same copy.
	if !copyUsable(rt, exe) {
		if err := copyExecutable(exe, rt); err != nil {
			return err
		}
	}
	pruneCopies(filepath.Dir(rt), rt)
	// Remember where we came from: the resident daemon watches that file for
	// upgrades and hands off to it.
	_ = os.Setenv(InstalledBinEnv, exe)
	return reexecTo(rt)
}

// copyUsable reports whether an existing runtime copy can be exec'd in place
// of exe: same size, still executable, and no partial write left behind.
func copyUsable(rt, exe string) bool {
	rf, err := os.Stat(rt)
	if err != nil {
		return false
	}
	ef, err := os.Stat(exe)
	if err != nil {
		return false
	}
	return rf.Size() == ef.Size() && rf.Mode().IsRegular() && rf.Mode()&0o111 != 0
}

// pruneCopies removes runtime copies in dir that no longer correspond to the
// one this process uses, once they are older than staleCopyAge. Unlinking a
// copy another process is running from is safe on Unix: the running image
// keeps the inode alive, and nothing re-reads the path.
func pruneCopies(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleCopyAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), copyPattern) {
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

// isRuntimeCopy reports whether path is a runtime copy: a file directly in
// the runtime directory under the copy-name pattern.
func isRuntimeCopy(path string) bool {
	dir, err := RuntimeDir()
	if err != nil {
		return false
	}
	return filepath.Dir(path) == dir && strings.HasPrefix(filepath.Base(path), copyPattern)
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
func copyExecutable(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
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
