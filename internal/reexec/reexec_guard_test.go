package reexec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scratchRuntime points RuntimePath at a scratch directory without the
// t.Setenv of runtimeRoot, for tests that need to set particular variables
// themselves.
func scratchRuntime(t *testing.T) (dir, rt string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir, filepath.Join(dir, "gh-monitor", "gh-monitor")
}

// stubReexec replaces the exec with a recorder, so a test can drive
// MaybeReexec through the hop it would otherwise take — and outlive it.
func stubReexec(t *testing.T) *[]string {
	t.Helper()
	orig := reexecToFn
	calls := []string{}
	reexecToFn = func(path string) error {
		calls = append(calls, path)
		return nil
	}
	t.Cleanup(func() { reexecToFn = orig })
	return &calls
}

// TestMaybeReexecHopsOnceAndRecordsItsDepth pins the accounting the bound
// depends on: a hop increments the depth, so a chain can be counted at all.
func TestMaybeReexecHopsOnceAndRecordsItsDepth(t *testing.T) {
	_, rt := scratchRuntime(t)
	calls := stubReexec(t)
	t.Setenv(ReexecDepthEnv, "0")

	if err := MaybeReexec(); err != nil {
		t.Fatalf("MaybeReexec: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != rt {
		t.Fatalf("hops = %v, want exactly one to %q", *calls, rt)
	}
	if got := os.Getenv(ReexecDepthEnv); got != "1" {
		t.Fatalf("%s after one hop = %q, want \"1\"", ReexecDepthEnv, got)
	}
}

// TestMaybeReexecRefusesToHopForever is the bound issue #196 asks for. The
// identity check alone is not enough: it holds only while every hop lands on
// the file the previous one wrote, and the failure that filled /tmp with 337 GB
// was exactly that assumption breaking — each generation copied again, and
// each kill on the helper's own -test.timeout left its temp file behind.
func TestMaybeReexecRefusesToHopForever(t *testing.T) {
	dir, _ := scratchRuntime(t)
	calls := stubReexec(t)
	t.Setenv(ReexecDepthEnv, strconv.Itoa(maxReexecDepth))

	err := MaybeReexec()
	if err == nil {
		t.Fatal("MaybeReexec at the depth limit must refuse, and say so")
	}
	if !strings.Contains(err.Error(), ReexecDepthEnv) {
		t.Fatalf("error %q should name %s, so the cause is visible in a log", err, ReexecDepthEnv)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused at the depth limit but hopped anyway: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "gh-monitor", "gh-monitor")); !os.IsNotExist(err) {
		t.Fatalf("a refused hop must not leave a copy behind; stat err = %v", err)
	}
}

// TestMaybeReexecDepthSurvivesGarbage: an unreadable depth is not a reason to
// refuse to start. Start counting from zero and the bound still applies.
func TestMaybeReexecDepthSurvivesGarbage(t *testing.T) {
	for _, value := range []string{"", "abc", "-1", "one", "+"} {
		t.Run(value, func(t *testing.T) {
			scratchRuntime(t)
			stubReexec(t)
			t.Setenv(ReexecDepthEnv, value)
			if err := MaybeReexec(); err != nil {
				t.Fatalf("MaybeReexec with %s=%q: %v", ReexecDepthEnv, value, err)
			}
		})
	}
}

// TestMaybeReexecReadsAPaddedDepth: the depth is a number, and being wrapped in
// spaces does not make it a different number.
func TestMaybeReexecReadsAPaddedDepth(t *testing.T) {
	scratchRuntime(t)
	calls := stubReexec(t)
	t.Setenv(ReexecDepthEnv, " 2 ")

	if err := MaybeReexec(); err == nil {
		t.Fatalf("a padded %s at the limit must still refuse", ReexecDepthEnv)
	}
	if len(*calls) != 0 {
		t.Fatalf("hopped despite the limit: %v", *calls)
	}
}

// TestCopyExecutableReclaimsAbandonedCopies covers the debris the bound leaves
// behind: a copy killed mid-write never gets to rename its temp file, and
// nothing else will ever remove it.
func TestCopyExecutableReclaimsAbandonedCopies(t *testing.T) {
	_, rt := scratchRuntime(t)
	runtimeDir := filepath.Dir(rt)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(runtimeDir, "gh-monitor.tmp-0123456789")
	if err := os.WriteFile(abandoned, []byte("half a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleCopyAge)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}
	// A copy in flight right now is somebody else's hop. Leave it alone: a
	// successful copy takes milliseconds, so an age test never mistakes one for
	// debris, and removing it would turn that hop into a failure for no reason.
	inFlight := filepath.Join(runtimeDir, "gh-monitor.tmp-live")
	if err := os.WriteFile(inFlight, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	src, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := copyExecutable(src, rt); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned copy %q survived; stat err = %v", abandoned, err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("in-flight copy was removed: %v", err)
	}
	if _, err := os.Stat(rt); err != nil {
		t.Fatalf("runtime copy missing: %v", err)
	}
}

// TestCopyExecutableToleratesConcurrentAttempts: several clients starting at
// once each leave a copy mid-write for a moment. That is ordinary, and refusing
// them would put the installed binary back on the mapped list.
func TestCopyExecutableToleratesConcurrentAttempts(t *testing.T) {
	_, rt := scratchRuntime(t)
	runtimeDir := filepath.Dir(rt)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		path := filepath.Join(runtimeDir, "gh-monitor.tmp-concurrent"+strconv.Itoa(i))
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := copyExecutable(src, rt); err != nil {
		t.Fatalf("copyExecutable alongside concurrent attempts: %v", err)
	}
	if _, err := os.Stat(rt); err != nil {
		t.Fatalf("runtime copy missing: %v", err)
	}
}

// TestCopyExecutableRefusesAnUnhealthyRuntimeDir is the second line of defence:
// when the directory is already full of interrupted copies, adding another one
// makes the situation worse and hides the cause. Say so instead.
func TestCopyExecutableRefusesAnUnhealthyRuntimeDir(t *testing.T) {
	_, rt := scratchRuntime(t)
	runtimeDir := filepath.Dir(rt)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse, so the test costs no disk: the guard must read the sizes, not
	// write that many bytes to find out.
	bloated := filepath.Join(runtimeDir, "gh-monitor.tmp-bloated")
	f, err := os.Create(bloated)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(maxPendingCopyFactor+1) * info.Size()); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	err = copyExecutable(src, rt)
	if err == nil {
		t.Fatalf("copyExecutable must refuse a runtime dir holding %d bytes of interrupted copies",
			int64(maxPendingCopyFactor+1)*info.Size())
	}
	if !strings.Contains(err.Error(), runtimeDir) {
		t.Fatalf("error %q should name %s, so the operator knows where to look", err, runtimeDir)
	}
	if _, err := os.Stat(rt); !os.IsNotExist(err) {
		t.Fatalf("a refused copy must not install anything; stat err = %v", err)
	}
}
