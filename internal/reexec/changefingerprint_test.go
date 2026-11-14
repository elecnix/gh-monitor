//go:build linux || darwin

package reexec

import (
	"os"
	"path/filepath"
	"testing"
)

// TestChangeFingerprintSeesAnInPlaceRewrite pins why the upgrade watcher uses
// ChangeFingerprint: rewriting a file in place with the same size and its
// mtime set back leaves Fingerprint unchanged, and moves the inode change
// time (issue #216).
func TestChangeFingerprintSeesAnInPlaceRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, []byte("v1-body"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, _ := Fingerprint(path)
	beforeChange, _ := ChangeFingerprint(path)
	beforeSum, _ := ContentSum(path)

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("v2-body"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	afterStat, _ := Fingerprint(path)
	afterChange, _ := ChangeFingerprint(path)
	afterSum, _ := ContentSum(path)
	if afterStat != beforeStat {
		t.Fatalf("precondition: Fingerprint was expected to miss this rewrite")
	}
	if afterChange == beforeChange {
		t.Errorf("ChangeFingerprint must change on an in-place rewrite that keeps size and mtime")
	}
	if afterSum == beforeSum {
		t.Errorf("ContentSum must change when the bytes do")
	}
}

// TestContentSumReportsAReadFailure keeps a missing file from hashing as
// anything: the caller must see the error.
func TestContentSumReportsAReadFailure(t *testing.T) {
	if _, err := ContentSum(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("ContentSum of a missing file must return an error")
	}
}
