package reexec

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runtimeCopyOf points RuntimePath at a scratch directory and names the file the
// resident copy lands on. The name is content-addressed, so it is asked for
// rather than assumed.
func runtimeCopyOf(t *testing.T) (dir, rt string) {
	t.Helper()
	dir = runtimeRoot(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	rt, err = CopyPathFor(exe)
	if err != nil {
		t.Fatalf("CopyPathFor: %v", err)
	}
	return dir, rt
}

// prepareRuntime is the starting point of every copyExecutable test: a scratch
// runtime directory that exists, and the binary to copy into it.
func prepareRuntime(t *testing.T) (runtimeDir, rt, src string) {
	t.Helper()
	_, rt = runtimeCopyOf(t)
	runtimeDir = filepath.Dir(rt)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return runtimeDir, rt, src
}

// leaveCopy puts an interrupted copy of size bytes in the runtime directory and
// backdates it, so age-based reclamation can be exercised deliberately. A size
// of zero writes a small file; anything larger is sparse and costs no disk.
func leaveCopy(t *testing.T, runtimeDir, name string, size int64, age time.Duration) string {
	t.Helper()
	// Debris is named after the copy it was interrupted while writing, and the
	// copy's name is content-addressed: one build's debris is not another's.
	dst, err := CopyPathFor(mustExecutable(t))
	if err != nil {
		t.Fatalf("CopyPathFor: %v", err)
	}
	path := filepath.Join(runtimeDir, filepath.Base(dst)+".tmp-"+name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		size = 6 // "debris"
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if age != 0 {
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%q survived; stat err = %v", path, err)
	}
}

func requirePresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%q missing: %v", path, err)
	}
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

// TestChildEnvDropsTheDepth: a spawned process starts its own chain, so the hop
// count of the chain that spawned it must not travel with it. Everything else in
// the environment belongs to the operator and is passed through untouched.
func TestChildEnvDropsTheDepth(t *testing.T) {
	t.Setenv(ReexecDepthEnv, "1")
	t.Setenv("GH_MONITOR_PROBE_KEPT", "kept")

	env := ChildEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, ReexecDepthEnv+"=") {
			t.Fatalf("%s leaked to the child: %q", ReexecDepthEnv, kv)
		}
	}
	if !slices.Contains(env, "GH_MONITOR_PROBE_KEPT=kept") {
		t.Fatal("ChildEnv dropped an unrelated variable")
	}
}

// TestMaybeReexecHopsOnceAndRecordsItsDepth pins the accounting the bound
// depends on: a hop increments the depth, so a chain can be counted at all.
func TestMaybeReexecHopsOnceAndRecordsItsDepth(t *testing.T) {
	_, rt := runtimeCopyOf(t)
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
	_, rt := runtimeCopyOf(t)
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
	requireGone(t, rt)
}

// TestMaybeReexecDepthSurvivesGarbage: an unreadable depth is not a reason to
// refuse to start. Start counting from zero and the bound still applies.
func TestMaybeReexecDepthSurvivesGarbage(t *testing.T) {
	for _, value := range []string{"", "abc", "-1", "one", "+"} {
		t.Run(value, func(t *testing.T) {
			_, rt := runtimeCopyOf(t)
			stubReexec(t)
			t.Setenv(ReexecDepthEnv, value)
			if err := MaybeReexec(); err != nil {
				t.Fatalf("MaybeReexec with %s=%q: %v", ReexecDepthEnv, value, err)
			}
			requirePresent(t, rt)
		})
	}
}

// TestMaybeReexecReadsAPaddedDepth: the depth is a number, and being wrapped in
// spaces does not make it a different number.
func TestMaybeReexecReadsAPaddedDepth(t *testing.T) {
	_, rt := runtimeCopyOf(t)
	calls := stubReexec(t)
	t.Setenv(ReexecDepthEnv, " 2 ")

	if err := MaybeReexec(); err == nil {
		t.Fatalf("a padded %s at the limit must still refuse", ReexecDepthEnv)
	}
	if len(*calls) != 0 {
		t.Fatalf("hopped despite the limit: %v", *calls)
	}
	requireGone(t, rt)
}

// TestCopyExecutableReclaimsAbandonedCopies covers the debris the bound leaves
// behind: a copy killed mid-write never gets to rename its temp file, and
// nothing else will ever remove it.
func TestCopyExecutableReclaimsAbandonedCopies(t *testing.T) {
	runtimeDir, rt, src := prepareRuntime(t)
	abandoned := leaveCopy(t, runtimeDir, "abandoned", 0, 2*staleCopyAge)
	// A copy in flight right now is somebody else's hop. Leave it alone: a
	// successful copy takes milliseconds, so an age test never mistakes one for
	// debris, and removing it would turn that hop into a failure for no reason.
	inFlight := leaveCopy(t, runtimeDir, "live", 0, 0)

	if err := copyExecutable(src, rt); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}
	requireGone(t, abandoned)
	requirePresent(t, inFlight)
	requirePresent(t, rt)
}

// TestCopyExecutableToleratesConcurrentAttempts: several clients starting at
// once each leave a copy mid-write for a moment. That is ordinary, and refusing
// them would put the installed binary back on the mapped list.
func TestCopyExecutableToleratesConcurrentAttempts(t *testing.T) {
	runtimeDir, rt, src := prepareRuntime(t)
	for i := 0; i < 4; i++ {
		leaveCopy(t, runtimeDir, "concurrent"+strconv.Itoa(i), 0, 0)
	}

	if err := copyExecutable(src, rt); err != nil {
		t.Fatalf("copyExecutable alongside concurrent attempts: %v", err)
	}
	requirePresent(t, rt)
}

// TestCopyExecutableRefusesAnUnhealthyRuntimeDir is the second line of defence:
// when the directory is already full of interrupted copies, adding another one
// makes the situation worse and hides the cause. Say so instead.
func TestCopyExecutableRefusesAnUnhealthyRuntimeDir(t *testing.T) {
	runtimeDir, rt, src := prepareRuntime(t)
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	bloated := int64(maxPendingCopyFactor+1) * info.Size()
	leaveCopy(t, runtimeDir, "bloated", bloated, 0)

	err = copyExecutable(src, rt)
	if err == nil {
		t.Fatalf("copyExecutable must refuse a runtime dir holding %d bytes of interrupted copies", bloated)
	}
	if !strings.Contains(err.Error(), runtimeDir) {
		t.Fatalf("error %q should name %s, so the operator knows where to look", err, runtimeDir)
	}
	requireGone(t, rt)
}

// mustExecutable is os.Executable with a test-friendly failure mode.
func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}
