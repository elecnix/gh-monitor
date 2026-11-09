package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/elecnix/gh-monitor/internal/cursor"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #219: a named watch persists its cursor only after the updates it
// covers were written. A process killed between the two steps must leave the
// cursor where the consumer last confirmed, so the next run with the same
// --instance emits those updates again.

const cursorTestInstance = "cursor-after-write"

// cursorTestSetup points the config dir at a temp dir and serves an open PR
// with one failing check. It returns the cursor store the command writes to.
func cursorTestSetup(t *testing.T) *cursor.DiskStore {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("GH_HOST", "")
	originalFactory := apiClientFactory
	t.Cleanup(func() { apiClientFactory = originalFactory })
	fake := &commandFakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
		return assignJSON(result, openPRWithFailingCheck())
	}}
	apiClientFactory = func(string) ghcli.API { return fake }
	store, err := cursor.NewDiskStore(filepath.Join(cfg, "gh-monitor"))
	require.NoError(t, err)
	return store
}

// runInstance runs one `--once --instance` tick with the given writer as
// stdout and extra flags, and returns the command's error.
func runInstance(t *testing.T, out interface{ Write([]byte) (int, error) }, extra ...string) error {
	t.Helper()
	root := newRootCommand()
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"7", "-R", "o/r", "--once", "--instance", cursorTestInstance}, extra...))
	return root.Execute()
}

// eventTypes runs one tick into a buffer and returns the NDJSON event types.
func eventTypes(t *testing.T, extra ...string) []string {
	t.Helper()
	stdout := &bytes.Buffer{}
	require.NoError(t, runInstance(t, stdout, extra...))
	var types []string
	for _, ln := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if ln == "" {
			continue
		}
		var n map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(ln), &n), "line not valid json: %s", ln)
		types = append(types, n["type"].(string))
	}
	return types
}

// cursorExists reports whether the instance has a stored cursor.
func cursorExists(t *testing.T, store *cursor.DiskStore) bool {
	t.Helper()
	_, err := store.Load(cursorTestInstance)
	if os.IsNotExist(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// probeWriter records, at each write, whether the cursor was already stored.
// A write is the last moment a kill could interrupt before the consumer has
// the line, so the cursor at that moment is what the next run would start
// from.
type probeWriter struct {
	t       *testing.T
	store   *cursor.DiskStore
	seen    []bool
	failAt  int // 1-based write that fails; 0 never fails
	written bytes.Buffer
}

func (w *probeWriter) Write(p []byte) (int, error) {
	w.seen = append(w.seen, cursorExists(w.t, w.store))
	if w.failAt > 0 && len(w.seen) >= w.failAt {
		return 0, syscall.EPIPE
	}
	return w.written.Write(p)
}

func TestInstanceCursorAdvancesAfterWrittenEvents(t *testing.T) {
	store := cursorTestSetup(t)

	assert.Contains(t, eventTypes(t), "new-failing-checks")
	assert.True(t, cursorExists(t, store), "a written batch advances the cursor")
	assert.NotContains(t, eventTypes(t), "new-failing-checks",
		"the next run diffs against the advanced cursor")
}

func TestInstanceCursorNotStoredWhileTheBatchIsWritten(t *testing.T) {
	store := cursorTestSetup(t)

	w := &probeWriter{t: t, store: store}
	require.NoError(t, runInstance(t, w))
	require.NotEmpty(t, w.seen)
	for i, stored := range w.seen {
		assert.False(t, stored, "write %d ran after the cursor was stored: a kill here loses the update", i+1)
	}
	assert.True(t, cursorExists(t, store), "the cursor is stored once the batch is written")
}

func TestInstanceCursorAdvancesPastSuppressedEvents(t *testing.T) {
	store := cursorTestSetup(t)

	// The --events allowlist suppresses every event this tick produces.
	assert.Empty(t, eventTypes(t, "--events", "merged"))
	assert.True(t, cursorExists(t, store), "a suppressed event is a decision, so it advances the cursor")
	assert.NotContains(t, eventTypes(t), "new-failing-checks",
		"a suppressed event is not replayed on the next run")
}

func TestInstanceCursorHeldOnFailedWrite(t *testing.T) {
	store := cursorTestSetup(t)

	_ = runInstance(t, &brokenWriter{err: syscall.EPIPE})
	assert.False(t, cursorExists(t, store), "a failed write must not advance the cursor")
	assert.Contains(t, eventTypes(t), "new-failing-checks",
		"the next run emits the update the failed write lost")
}

func TestInstanceCursorHeldOnFailedWriteMidBatch(t *testing.T) {
	store := cursorTestSetup(t)

	// The first line of the batch is written and the second fails: every
	// update in a batch carries the same post-poll state, so storing it
	// after the first line would skip the second.
	w := &probeWriter{t: t, store: store, failAt: 2}
	_ = runInstance(t, w)
	require.GreaterOrEqual(t, len(w.seen), 2, "the batch has at least two lines")
	assert.False(t, cursorExists(t, store), "a batch with a failed write must not advance the cursor")
	assert.Contains(t, eventTypes(t), "new-failing-checks")
}

func TestInstanceCursorHeldOnFailedUntilWrite(t *testing.T) {
	store := cursorTestSetup(t)

	// The --until member is written directly, past the --events filter.
	_ = runInstance(t, &brokenWriter{err: syscall.EPIPE}, "--until", "new-failing-checks")
	assert.False(t, cursorExists(t, store), "a failed --until write must not advance the cursor")
	assert.Contains(t, eventTypes(t), "new-failing-checks")
}

func TestInstanceCursorAdvancesAfterWrittenUntilBatch(t *testing.T) {
	store := cursorTestSetup(t)

	w := &probeWriter{t: t, store: store}
	require.NoError(t, runInstance(t, w, "--until", "new-failing-checks"))
	for i, stored := range w.seen {
		assert.False(t, stored, "write %d ran after the cursor was stored", i+1)
	}
	assert.True(t, cursorExists(t, store), "the batch a --until member ended is stored")
}

func TestInstanceCursorHeldOnFailedTextWrite(t *testing.T) {
	store := cursorTestSetup(t)

	_ = runInstance(t, &brokenWriter{err: syscall.EPIPE}, "--text")
	assert.False(t, cursorExists(t, store), "a failed --text write must not advance the cursor")
	assert.Contains(t, eventTypes(t), "new-failing-checks")
}
