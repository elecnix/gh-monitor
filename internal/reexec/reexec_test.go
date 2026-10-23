package reexec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helperEnv marks a test process as the re-exec helper child. The exec'd
// copy is the same test binary, so it sees the same constant.
const helperEnv = "GO_WANT_HELPER_REEXEC"

// runtimeRoot points RuntimePath at a scratch directory for the duration of
// the test, so tests never touch the real per-user runtime or cache dirs.
func runtimeRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir
}

func TestRuntimeDirPrefersXDGRuntimeDir(t *testing.T) {
	runtimeRoot(t)
	got, err := RuntimeDir()
	if err != nil {
		t.Fatalf("RuntimeDir: %v", err)
	}
	want := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "gh-monitor")
	if got != want {
		t.Fatalf("RuntimeDir() = %q, want %q", got, want)
	}
}

func TestRuntimeDirFallsBackToCacheDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", dir)
	got, err := RuntimeDir()
	if err != nil {
		t.Fatalf("RuntimeDir: %v", err)
	}
	want := filepath.Join(dir, "gh-monitor")
	if got != want {
		t.Fatalf("RuntimeDir() = %q, want %q", got, want)
	}
}

// TestRuntimePathIsContentAddressed pins what makes concurrent launches of
// different builds safe: the copy path is a function of the binary's
// identity, so it is stable for one build and different for another. A shared
// "current copy" name let one launch rewrite the file another was about to
// exec.
func TestRuntimePathIsContentAddressed(t *testing.T) {
	runtimeRoot(t)
	first, err := RuntimePath()
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}
	dir, err := RuntimeDir()
	if err != nil {
		t.Fatalf("RuntimeDir: %v", err)
	}
	if filepath.Dir(first) != dir {
		t.Fatalf("runtime copy %q is not in %q", first, dir)
	}
	if base := filepath.Base(first); !strings.HasPrefix(base, "gh-monitor-") || len(base) <= len("gh-monitor-") {
		t.Fatalf("runtime copy %q is not content-addressed", base)
	}
	// Stable for the same binary.
	again, err := RuntimePath()
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}
	if again != first {
		t.Fatalf("RuntimePath() changed for an unchanged binary: %q then %q", first, again)
	}
	// Different for a different binary, even at the same path.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	other := filepath.Join(t.TempDir(), "gh-monitor")
	src, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(other, src[:len(src)-1], 0o755); err != nil {
		t.Fatalf("write other binary: %v", err)
	}
	otherPath, err := CopyPathFor(other)
	if err != nil {
		t.Fatalf("CopyPathFor: %v", err)
	}
	if otherPath == first {
		t.Fatalf("two different binaries share one runtime copy path: %q", first)
	}
}

func TestMaybeReexecOptOutEnv(t *testing.T) {
	dir := runtimeRoot(t)
	t.Setenv(OptOutEnv, "0")
	if err := MaybeReexec(); err != nil {
		t.Fatalf("MaybeReexec with opt-out: %v", err)
	}
	// Nothing may have been copied.
	entries, err := os.ReadDir(filepath.Join(dir, "gh-monitor"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("opt-out run must not create the runtime copy, found %d entries", len(entries))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read runtime dir: %v", err)
	}
}

func TestMaybeReexecSkipsWhenAlreadyRuntimeCopy(t *testing.T) {
	runtimeRoot(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	rt, err := CopyPathFor(exe)
	if err != nil {
		t.Fatalf("CopyPathFor: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(rt), 0o700); err != nil {
		t.Fatal(err)
	}
	// Hard-link the test binary at its runtime path: same inode, so SameFile
	// must short-circuit the copy+exec even without any marker env var.
	if err := os.Link(exe, rt); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := MaybeReexec(); err != nil {
		t.Fatalf("MaybeReexec on an already-runtime binary: %v", err)
	}
	// Reaching this line at all proves the process was not replaced.
}

func TestMaybeReexecCopiesAndExecs(t *testing.T) {
	dir := runtimeRoot(t)
	rt, err := RuntimePath()
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestReexecHelperProcess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		"XDG_RUNTIME_DIR="+dir,
		"XDG_CACHE_HOME="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "REEXEC-OK") {
		t.Fatalf("helper did not report a successful re-exec:\n%s", out)
	}

	// The runtime copy must exist, be executable, and match the original.
	orig, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	want, err := os.ReadFile(orig)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	got, err := os.ReadFile(rt)
	if err != nil {
		t.Fatalf("read runtime copy: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("runtime copy differs from the original binary")
	}
	info, err := os.Stat(rt)
	if err != nil {
		t.Fatalf("stat runtime copy: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("runtime copy is not executable: %v", info.Mode())
	}
}

// TestMaybeReexecReleasesInstalledBinary is the issue #73 end-to-end shape:
// while a process launched from an "installed" path is resident, the
// installed file itself must be writable in place — the whole point of
// running the resident worker from a runtime copy.
func TestMaybeReexecReleasesInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	src, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(installed, src, 0o755); err != nil {
		t.Fatalf("write installed binary: %v", err)
	}

	cmd := exec.Command(installed, "-test.run=^TestReexecHelperProcess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		"XDG_RUNTIME_DIR="+dir,
		"XDG_CACHE_HOME="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "REEXEC-OK") {
		t.Fatalf("helper did not report a successful re-exec:\n%s", out)
	}

	// The installed path is no longer mapped by a live image: an in-place
	// truncate+write (what `gh extension upgrade` does) must succeed.
	f, err := os.OpenFile(installed, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatalf("in-place write to the installed binary while resident: %v", err)
	}
	_ = f.Close()
}

// TestReexecHelperProcess is never run as a test directly. Invoked as a child
// process (helperEnv set), it calls MaybeReexec: the first time through it
// copies the binary and syscall.Exec's the runtime copy, which re-enters this
// same helper; the second time the executable already IS the runtime copy, so
// MaybeReexec returns nil and the helper proves where it ended up.
func TestReexecHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process for TestMaybeReexecCopiesAndExecs")
	}
	dir, err := RuntimeDir()
	if err != nil {
		t.Fatalf("RuntimeDir: %v", err)
	}
	if err := MaybeReexec(); err != nil {
		t.Fatalf("MaybeReexec: %v", err)
	}
	after, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// The copy's own path is keyed by the copy's identity, which is not the
	// path it was written to, so the assertion is about where it lives.
	if filepath.Dir(after) != dir || !strings.HasPrefix(filepath.Base(after), "gh-monitor-") {
		t.Errorf("running from %q, want a runtime copy in %q", after, dir)
	}
	procExe, err := os.Readlink("/proc/self/exe")
	if err == nil && procExe != after {
		t.Errorf("/proc/self/exe = %q, want %q", procExe, after)
	}
	fmt.Println("REEXEC-OK")
}
