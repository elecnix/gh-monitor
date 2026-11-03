package ipc

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBindResult_RoundTrip covers the file the spawned daemon writes and the
// client reads. The answer has to survive the trip as one of two known
// values, and anything else has to be reported as unknown rather than guessed.
func TestBindResult_RoundTrip(t *testing.T) {
	for _, outcome := range []string{BindWon, BindLost} {
		t.Run(outcome, func(t *testing.T) {
			path, err := NewBindResultPath()
			require.NoError(t, err)
			t.Cleanup(func() { RemoveBindResult(path) })

			// Before the daemon reports, the answer is unknown.
			_, err = ReadBindResult(path)
			assert.ErrorIs(t, err, ErrBindResult, "an unreported outcome must read as unknown")

			WriteBindResultTo(path, outcome)
			got, err := ReadBindResult(path)
			require.NoError(t, err)
			assert.Equal(t, outcome, got)
		})
	}
}

// TestBindResult_UnknownOutcome checks that a value the package does not
// recognise is reported as unknown. Guessing "won" from a corrupted file
// would silence the notice for a daemon that never served.
func TestBindResult_UnknownOutcome(t *testing.T) {
	path, err := NewBindResultPath()
	require.NoError(t, err)
	t.Cleanup(func() { RemoveBindResult(path) })

	require.NoError(t, os.WriteFile(path, []byte("something else"), 0o600))
	_, err = ReadBindResult(path)
	assert.ErrorIs(t, err, ErrBindResult, "an unrecognised outcome must read as unknown")
}

// TestBindResult_MissingFileIsUnknown checks the missing-file case: a daemon
// that died before it could report leaves no answer, and the client must
// treat that as unknown rather than as an error.
func TestBindResult_MissingFileIsUnknown(t *testing.T) {
	_, err := ReadBindResult(t.TempDir() + "/never-written.result")
	assert.ErrorIs(t, err, ErrBindResult)
}

// TestWaitForBindResult_WaitsForTheReport covers the gap the wait exists for:
// the socket can answer before the report lands, so reading once would
// mistake that gap for an unknown answer.
func TestWaitForBindResult_WaitsForTheReport(t *testing.T) {
	path, err := NewBindResultPath()
	require.NoError(t, err)
	t.Cleanup(func() { RemoveBindResult(path) })

	go func() {
		time.Sleep(50 * time.Millisecond)
		WriteBindResultTo(path, BindLost)
	}()

	got, err := WaitForBindResult(path, 5*time.Second)
	require.NoError(t, err, "the wait must pick up a late report")
	assert.Equal(t, BindLost, got)
}

// TestWaitForBindResult_GivesUp checks the bounded wait: a daemon that never
// reports must not hold the client.
func TestWaitForBindResult_GivesUp(t *testing.T) {
	path, err := NewBindResultPath()
	require.NoError(t, err)
	t.Cleanup(func() { RemoveBindResult(path) })

	start := time.Now()
	_, err = WaitForBindResult(path, 150*time.Millisecond)
	assert.ErrorIs(t, err, ErrBindResult)
	assert.Less(t, time.Since(start), 5*time.Second, "the wait must be bounded")
}

// TestEnvWithBindResult checks the environment handling. A process that
// already carries the variable — a daemon spawning a successor — must not
// leave two entries behind: the child would read the first one it finds,
// which is not the answer the client is waiting for.
func TestEnvWithBindResult(t *testing.T) {
	env := []string{"PATH=/usr/bin", BindResultEnv + "=/tmp/old.result", "HOME=/root"}

	got := EnvWithBindResult(env, "/tmp/new.result")
	assert.Contains(t, got, BindResultEnv+"=/tmp/new.result")
	assert.NotContains(t, got, BindResultEnv+"=/tmp/old.result",
		"the stale value must be replaced, not shadowed")
	assert.Contains(t, got, "PATH=/usr/bin", "the rest of the environment must survive")
	assert.Len(t, got, 3)

	// An empty path removes the variable entirely: a sub-daemon must not
	// report into the file the client that spawned the daemon is waiting on.
	cleared := EnvWithBindResult(env, "")
	for _, kv := range cleared {
		assert.NotContains(t, kv, BindResultEnv+"=")
	}
	assert.Len(t, cleared, 2)
}
