package fsclock

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWaitForTimestampTickMovesTheNextWrite pins the helper's promise: a
// write made right after it returns gets a later stamp than the file had.
// Without the wait, an immediate rewrite keeps the same stamp most of the
// time on a kernel with a coarse timestamp clock (issue #237). It repeats, so
// one lucky tick boundary cannot pass it.
func TestWaitForTimestampTickMovesTheNextWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		WaitForTimestampTick(t, path)

		if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !stamp(after).After(stamp(before)) {
			t.Fatalf("try %d: the write after the wait kept stamp %v", i, stamp(after))
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the probe file must be removed, found %d entries", len(entries))
	}
}
