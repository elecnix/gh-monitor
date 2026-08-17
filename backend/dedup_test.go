package backend

import (
	"strconv"
	"testing"
)

func TestDeduperDropsARepeatedID(t *testing.T) {
	d := NewDeduper(0)
	u := Update{ID: "delivery-1"}
	if !d.Allow(u) {
		t.Fatal("the first delivery must pass")
	}
	if d.Allow(u) {
		t.Fatal("a redelivery of the same ID must be dropped")
	}
}

func TestDeduperAllowsDistinctIDs(t *testing.T) {
	d := NewDeduper(0)
	for i := 0; i < 10; i++ {
		if !d.Allow(Update{ID: strconv.Itoa(i)}) {
			t.Fatalf("distinct ID %d was dropped", i)
		}
	}
}

func TestDeduperAlwaysAllowsAnEmptyID(t *testing.T) {
	// An empty ID is a backend saying it cannot identify its updates. Treating
	// them all as one would silently collapse unrelated changes.
	d := NewDeduper(0)
	for i := 0; i < 3; i++ {
		if !d.Allow(Update{}) {
			t.Fatal("an update with no ID must always be delivered")
		}
	}
}

func TestDeduperWindowIsBounded(t *testing.T) {
	// Memory has to stay flat on a watch that runs for weeks, so the window
	// forgets the oldest IDs. What it forgets it will deliver again, which is
	// the right trade: a redelivery arrives close behind its original.
	d := NewDeduper(2)
	d.Allow(Update{ID: "a"})
	d.Allow(Update{ID: "b"})
	d.Allow(Update{ID: "c"}) // evicts "a"

	if d.Allow(Update{ID: "c"}) {
		t.Fatal("the newest ID should still be remembered")
	}
	if d.Allow(Update{ID: "b"}) {
		t.Fatal("the second-newest ID should still be remembered")
	}
	if len(d.seen) > 2 {
		t.Fatalf("window grew to %d entries, want at most 2", len(d.seen))
	}
}
