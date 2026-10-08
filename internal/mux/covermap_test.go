package mux

import (
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend/remote"
)

func TestCoverageMapRepositoryNamesIgnoreCase(t *testing.T) {
	m := NewCoverageMap(time.Hour, time.Minute)
	m.Apply("d", remote.CoverageEntry{Repo: "Owner/Repo", Covered: true})
	if d, _, ok := m.Covered("owner", "REPO"); !ok || d != "d" {
		t.Fatalf("Covered = %q, %v", d, ok)
	}
}

func TestCoverageMapExportImportCarriesCoverageAcrossAHandoff(t *testing.T) {
	a := NewCoverageMap(time.Hour, time.Minute)
	a.Apply("d", remote.CoverageEntry{Repo: "o/r", Covered: true})
	b := NewCoverageMap(time.Hour, time.Minute)
	b.Import(a.Export())
	if _, _, ok := b.Covered("o", "r"); !ok {
		t.Fatal("the successor must inherit the covered repository")
	}
}

func TestCoverageMapRetainDropsWhatTheStreamDidNotRepeat(t *testing.T) {
	m := NewCoverageMap(time.Hour, time.Minute)
	m.Apply("d", remote.CoverageEntry{Repo: "o/old", Covered: true})
	m.Apply("d", remote.CoverageEntry{Repo: "o/kept", Covered: true})
	m.Retain("d", map[string]bool{"o/kept": true})
	if _, _, ok := m.Covered("o", "old"); ok {
		t.Fatal("an entry the daemon no longer vouches for must go")
	}
	if _, _, ok := m.Covered("o", "kept"); !ok {
		t.Fatal("a repeated entry must stay")
	}
}
