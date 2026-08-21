package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sampleEvent struct {
	ID string `json:"id"`
	Pr string `json:"pr"`
	At string `json:"at"`
}

// setDay pins the writer's clock so rotation can be exercised without
// waiting for midnight.
func setDay(t *testing.T, day string) {
	t.Helper()
	at, err := time.Parse("2006-01-02", day)
	require.NoError(t, err)
	orig := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = orig })
}

func TestLogWritesJSONLAppendOnly(t *testing.T) {
	dir := t.TempDir()
	setDay(t, "2026-08-21")
	w, err := New(dir, 10)
	require.NoError(t, err)

	require.NoError(t, w.Log(sampleEvent{ID: "1", Pr: "o/r#1", At: "t1"}))
	require.NoError(t, w.Log(sampleEvent{ID: "2", Pr: "o/r#2", At: "t2"}))
	require.NoError(t, w.Close())

	f, err := os.Open(filepath.Join(dir, "events-2026-08-21.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		require.True(t, json.Valid([]byte(line)), "each line must be valid JSON: %q", line)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		lines = append(lines, m)
	}
	require.NoError(t, sc.Err())
	require.Len(t, lines, 2)
	assert.Equal(t, "o/r#1", lines[0]["pr"])
	assert.Equal(t, "o/r#2", lines[1]["pr"])

	// Re-opening the same day appends — never truncates.
	setDay(t, "2026-08-21")
	w2, err := New(dir, 10)
	require.NoError(t, err)
	require.NoError(t, w2.Log(sampleEvent{ID: "3", Pr: "o/r#3", At: "t3"}))
	require.NoError(t, w2.Close())
	data, err := os.ReadFile(filepath.Join(dir, "events-2026-08-21.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(string(data), "\n"), "append-only: three lines after reopen")
}

func TestLogRotatesDailyByFilename(t *testing.T) {
	dir := t.TempDir()
	setDay(t, "2026-08-21")
	w, err := New(dir, 10)
	require.NoError(t, err)
	require.NoError(t, w.Log(sampleEvent{ID: "1", Pr: "day1", At: "t"}))

	setDay(t, "2026-08-22")
	require.NoError(t, w.Log(sampleEvent{ID: "2", Pr: "day2", At: "t"}))
	require.NoError(t, w.Close())

	d1, err := os.ReadFile(filepath.Join(dir, "events-2026-08-21.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(d1), "day1")
	assert.NotContains(t, string(d1), "day2", "yesterday's file must not gain new lines")

	d2, err := os.ReadFile(filepath.Join(dir, "events-2026-08-22.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(d2), "day2")
}

func TestRetentionDeletesFilesOlderThanKeepDays(t *testing.T) {
	dir := t.TempDir()
	setDay(t, "2026-08-31")
	keep := 10
	// Seed files spanning old-to-current. keep=10 keeps today (08-31) plus
	// the nine days before it, back to 08-22; anything older is pruned on
	// the next rotation check.
	old := []string{"events-2026-08-10.jsonl", "events-2026-08-20.jsonl", "events-2026-08-21.jsonl"}
	cur := []string{"events-2026-08-22.jsonl", "events-2026-08-30.jsonl", "events-2026-08-31.jsonl"}
	for _, name := range append(append([]string{}, old...), cur...) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644))
	}
	// Unrelated files must never be touched by pruning.
	unrelated := filepath.Join(dir, "unrelated.txt")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep me"), 0o644))

	w, err := New(dir, keep)
	require.NoError(t, err)
	require.NoError(t, w.Log(sampleEvent{ID: "1", Pr: "today", At: "t"}))
	require.NoError(t, w.Close())

	for _, name := range old {
		_, statErr := os.Stat(filepath.Join(dir, name))
		assert.True(t, os.IsNotExist(statErr), "%s should have been pruned", name)
	}
	for _, name := range cur {
		_, statErr := os.Stat(filepath.Join(dir, name))
		assert.NoError(t, statErr, "%s should be kept", name)
	}
	data, err := os.ReadFile(unrelated)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(data), "pruning must only touch events-*.jsonl")
}

func TestNewRejectsUnwritableDir(t *testing.T) {
	// A path under a regular file cannot become a directory.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err := New(filepath.Join(file, "events"), 10)
	require.Error(t, err)
}
