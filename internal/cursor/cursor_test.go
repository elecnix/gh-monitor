package cursor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemStore_SaveLoad(t *testing.T) {
	s := NewMemStore()
	c := Cursor{
		Instance: "orchestrator",
		Owner:    "elecnix",
		Repo:    "gh-monitor",
		Position: "2025-01-15T10:30:00Z",
		LastSeen: time.Now(),
	}
	require.NoError(t, s.Save(c))

	got, err := s.Load("orchestrator")
	require.NoError(t, err)
	assert.Equal(t, c.Instance, got.Instance)
	assert.Equal(t, c.Owner, got.Owner)
	assert.Equal(t, c.Repo, got.Repo)
	assert.Equal(t, c.Position, got.Position)
}

func TestMemStore_LoadNotFound(t *testing.T) {
	s := NewMemStore()
	_, err := s.Load("nonexistent")
	assert.True(t, os.IsNotExist(err))
}

func TestMemStore_Independent(t *testing.T) {
	// Two instances on the same repo have independent cursors; advancing one
	// does not affect the other.
	s := NewMemStore()
	c1 := Cursor{Instance: "orchestrator", Owner: "elecnix", Repo: "gh-monitor", Position: "2025-01-15T10:00:00Z"}
	c2 := Cursor{Instance: "agent-pr-957", Owner: "elecnix", Repo: "gh-monitor", Position: "2025-01-15T12:00:00Z"}
	require.NoError(t, s.Save(c1))
	require.NoError(t, s.Save(c2))

	// Advance orchestrator — agent-pr-957 should be unaffected.
	c1.Position = "2025-01-15T14:00:00Z"
	require.NoError(t, s.Save(c1))

	got1, _ := s.Load("orchestrator")
	got2, _ := s.Load("agent-pr-957")
	assert.Equal(t, "2025-01-15T14:00:00Z", got1.Position)
	assert.Equal(t, "2025-01-15T12:00:00Z", got2.Position)
}

func TestMemStore_Delete(t *testing.T) {
	s := NewMemStore()
	c := Cursor{Instance: "test", Owner: "elecnix", Repo: "gh-monitor"}
	require.NoError(t, s.Save(c))
	require.NoError(t, s.Delete("test"))
	_, err := s.Load("test")
	assert.True(t, os.IsNotExist(err))

	// Deleting a nonexistent cursor is a no-op.
	require.NoError(t, s.Delete("nonexistent"))
}

func TestMemStore_List(t *testing.T) {
	s := NewMemStore()
	c1 := Cursor{Instance: "b-instance", Owner: "elecnix", Repo: "gh-monitor"}
	c2 := Cursor{Instance: "a-instance", Owner: "elecnix", Repo: "gh-monitor"}
	require.NoError(t, s.Save(c1))
	require.NoError(t, s.Save(c2))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a-instance", list[0].Instance)
	assert.Equal(t, "b-instance", list[1].Instance)
}

func TestMemStore_ListEmpty(t *testing.T) {
	s := NewMemStore()
	list, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestDiskStore_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	c := Cursor{
		Instance: "orchestrator",
		Owner:    "elecnix",
		Repo:    "gh-monitor",
		Position: "2025-01-15T10:30:00Z",
		LastSeen: time.Now().Truncate(time.Second),
	}
	require.NoError(t, s.Save(c))

	got, err := s.Load("orchestrator")
	require.NoError(t, err)
	assert.Equal(t, c.Instance, got.Instance)
	assert.Equal(t, c.Owner, got.Owner)
	assert.Equal(t, c.Repo, got.Repo)
	assert.Equal(t, c.Position, got.Position)
	// LastSeen should be close (JSON serialisation may lose sub-second precision).
	assert.WithinDuration(t, c.LastSeen, got.LastSeen, time.Second)

	// Verify the file exists at the expected path.
	expectedPath := filepath.Join(s.Dir(), "orchestrator.json")
	_, err = os.Stat(expectedPath)
	require.NoError(t, err)
}

func TestDiskStore_LoadNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	_, err = s.Load("nonexistent")
	assert.True(t, os.IsNotExist(err))
}

func TestDiskStore_Independent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	c1 := Cursor{Instance: "orchestrator", Owner: "elecnix", Repo: "gh-monitor", Position: "2025-01-15T10:00:00Z"}
	c2 := Cursor{Instance: "agent-pr-957", Owner: "elecnix", Repo: "gh-monitor", Position: "2025-01-15T12:00:00Z"}
	require.NoError(t, s.Save(c1))
	require.NoError(t, s.Save(c2))

	c1.Position = "2025-01-15T14:00:00Z"
	require.NoError(t, s.Save(c1))

	got1, _ := s.Load("orchestrator")
	got2, _ := s.Load("agent-pr-957")
	assert.Equal(t, "2025-01-15T14:00:00Z", got1.Position)
	assert.Equal(t, "2025-01-15T12:00:00Z", got2.Position)
}

func TestDiskStore_Delete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	c := Cursor{Instance: "test", Owner: "elecnix", Repo: "gh-monitor"}
	require.NoError(t, s.Save(c))
	require.NoError(t, s.Delete("test"))
	_, err = s.Load("test")
	assert.True(t, os.IsNotExist(err))
}

func TestDiskStore_List(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	require.NoError(t, s.Save(Cursor{Instance: "b-instance", Owner: "elecnix", Repo: "gh-monitor"}))
	require.NoError(t, s.Save(Cursor{Instance: "a-instance", Owner: "elecnix", Repo: "gh-monitor"}))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a-instance", list[0].Instance)
	assert.Equal(t, "b-instance", list[1].Instance)
}

func TestDiskStore_ListSkipsTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	// Write a temp file — it should be skipped by List.
	tmpPath := filepath.Join(s.Dir(), "cursor-abc123.json.tmp")
	require.NoError(t, os.WriteFile(tmpPath, []byte(`{"instance":"ghost"}`), 0o644))

	require.NoError(t, s.Save(Cursor{Instance: "real", Owner: "elecnix", Repo: "gh-monitor"}))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "real", list[0].Instance)
}

func TestDiskStore_ListSkipsNonJSON(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(s.Dir(), "notes.txt"), []byte("not json"), 0o644))
	require.NoError(t, s.Save(Cursor{Instance: "real", Owner: "elecnix", Repo: "gh-monitor"}))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "real", list[0].Instance)
}

func TestDiskStore_SafeFilename(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir)
	require.NoError(t, err)

	c := Cursor{Instance: "agent/pr-957", Owner: "elecnix", Repo: "gh-monitor"}
	require.NoError(t, s.Save(c))

	// The file should exist with sanitised name.
	got, err := s.Load("agent/pr-957")
	require.NoError(t, err)
	assert.Equal(t, "agent/pr-957", got.Instance)

	// Verify the file on disk uses the sanitised name.
	_, err = os.Stat(filepath.Join(s.Dir(), "agent_pr-957.json"))
	require.NoError(t, err)
}

func TestSafeFilename(t *testing.T) {
	tests := []struct{ name, expected string }{
		{"orchestrator", "orchestrator"},
		{"agent-pr-957", "agent-pr-957"},
		{"a/b", "a_b"},
		{"hello world", "hello_world"},
		{"", "_"},
		{"///", "___"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.expected, safeFilename(tc.name), "safeFilename(%q)", tc.name)
	}
}
