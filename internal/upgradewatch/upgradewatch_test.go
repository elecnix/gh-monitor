package upgradewatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// base is a fixed mtime so tests can tell a real write from metadata churn
// without depending on filesystem timestamp granularity.
var base = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func writeBinary(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func newTracker(t *testing.T, path string) *Tracker {
	t.Helper()
	tr, err := New(path)
	if err != nil {
		t.Fatalf("New(%s): %v", path, err)
	}
	tr.MinGap = 0 // tests drive time by hand; the gap is exercised separately
	return tr
}

func TestUnchangedBinaryNeverChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-monitor")
	writeBinary(t, path, "binary-v1", base)
	tr := newTracker(t, path)

	for i := 0; i < 5; i++ {
		changed, err := tr.Poll()
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if changed {
			t.Fatalf("poll %d reported a change for an untouched binary", i)
		}
	}
}

func TestReplacedBinaryIsDetectedOnceSettled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-monitor")
	writeBinary(t, path, "binary-v1", base)
	tr := newTracker(t, path)

	// A new binary lands: a different size and mtime, exactly what an upgrade
	// write produces.
	writeBinary(t, path, "binary-v2-longer", base.Add(time.Minute))

	changed, err := tr.Poll()
	if err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if changed {
		t.Fatal("first sighting must only become a candidate, not a decision")
	}

	changed, err = tr.Poll()
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if !changed {
		t.Fatal("a settled, different binary must be reported")
	}

	// Adopted: the successor runs it, so it must not report again.
	changed, err = tr.Poll()
	if err != nil {
		t.Fatalf("third Poll: %v", err)
	}
	if changed {
		t.Fatal("the adopted binary must not be reported twice")
	}
}

func TestMetadataChurnIsNotAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-monitor")
	writeBinary(t, path, "binary-v1", base)
	tr := newTracker(t, path)

	// `touch`, rsync -t, a checkout that restores the same file: same bytes,
	// new mtime. Replacing a working watch for this would be pure churn.
	writeBinary(t, path, "binary-v1", base.Add(time.Hour))

	changed, err := tr.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if changed {
		t.Fatal("metadata-only change must stay a candidate on first sighting")
	}
	changed, err = tr.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if changed {
		t.Fatal("identical content must never be reported as a new binary")
	}
}

func TestPartialWriteThenRestoreIsNotAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-monitor")
	writeBinary(t, path, "binary-v1", base)
	tr := newTracker(t, path)

	// An upgrade in progress: the file is briefly a different, shorter thing.
	writeBinary(t, path, "bin", base.Add(time.Second))
	if changed, _ := tr.Poll(); changed {
		t.Fatal("a half-written binary must not be reported")
	}
	// The writer was interrupted and the file is back to what we run.
	writeBinary(t, path, "binary-v1", base.Add(2*time.Second))
	if changed, _ := tr.Poll(); changed {
		t.Fatal("a reverted candidate must not be reported")
	}
	if changed, _ := tr.Poll(); changed {
		t.Fatal("an unchanged binary must not be reported after churn")
	}
}

func TestMinGapPreventsThrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-monitor")
	writeBinary(t, path, "binary-v1", base)
	tr := newTracker(t, path)
	tr.MinGap = time.Hour

	writeBinary(t, path, "binary-v2-longer", base.Add(time.Minute))
	if changed, _ := tr.Poll(); changed {
		t.Fatal("first sighting must not be reported")
	}
	if changed, _ := tr.Poll(); !changed {
		t.Fatal("the first settled change must be reported")
	}

	// A second binary lands immediately after. One process must not replace
	// itself twice in a row — that is the shape of a relaunch loop.
	writeBinary(t, path, "binary-v3-much-longer", base.Add(2*time.Minute))
	if changed, _ := tr.Poll(); changed {
		t.Fatal("first sighting must not be reported")
	}
	if changed, _ := tr.Poll(); changed {
		t.Fatal("MinGap must suppress a second replacement inside the window")
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-monitor")
	writeBinary(t, path, "binary-v1", base)
	tr := newTracker(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	changed, err := tr.Poll()
	if err == nil {
		t.Fatal("a missing file must be reported as an error, not as no change")
	}
	if changed {
		t.Fatal("a missing file is not a replacement")
	}
}
