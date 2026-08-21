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
)

// OptOutEnv disables the relaunch-into-runtime-copy behaviour when set to "0".
// Every other value, including unset, keeps it on.
const OptOutEnv = "GH_MONITOR_REEXEC"

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
	rt, err := RuntimePath()
	if err != nil {
		return err
	}
	if sameFile(exe, rt) {
		return nil
	}
	if err := copyExecutable(exe, rt); err != nil {
		return err
	}
	return reexecTo(rt)
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
