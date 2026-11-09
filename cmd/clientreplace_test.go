package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientWatchForTest starts a client upgrader against installed with the check
// interval retuned for the test, and returns the upgrader plus a stop function
// that cancels it and waits for its goroutine to exit.
func clientWatchForTest(t *testing.T, installed string, stderr func(string)) (*clientUpgrader, func()) {
	t.Helper()
	origInterval := upgradeCheckInterval
	upgradeCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { upgradeCheckInterval = origInterval })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var up *clientUpgrader
	started := make(chan *clientUpgrader, 1)
	go func() {
		defer close(done)
		up = startClientUpgrade(ctx, writerFunc(stderr))
		started <- up
	}()
	return <-started, func() {
		cancel()
		<-done
	}
}

// captureExec substitutes the in-place exec and records its arguments. It
// returns a reader for the recorded calls.
func captureExec(t *testing.T) func() []execCall {
	t.Helper()
	var mu sync.Mutex
	var calls []execCall
	orig := execIntoFn
	t.Cleanup(func() { execIntoFn = orig })
	execIntoFn = func(path string, argv, env []string) error {
		mu.Lock()
		calls = append(calls, execCall{path: path, argv: argv, env: env})
		mu.Unlock()
		return nil
	}
	return func() []execCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]execCall(nil), calls...)
	}
}

type execCall struct {
	path string
	argv []string
	env  []string
}

// envValue reads one variable out of an exec environment slice as built for a
// child process, where a later duplicate would be ambiguous: the test wants
// exactly one value for it.
func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	value, found := "", false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value, found = strings.TrimPrefix(kv, prefix), true
		}
	}
	return value, found
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestWatchClientReplacesItselfOnBinaryChange is the point of issue #216: the
// installed binary changes, the resident watch re-execs into it, and the
// successor inherits the watch's ResumeID so the daemon hands that watcher's
// baseline back — no second first-poll, no replayed event.
func TestWatchClientReplacesItselfOnBinaryChange(t *testing.T) {
	t.Setenv(watchResumeIDEnv, "")
	t.Setenv(clientReplaceCountEnv, "")

	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
	t.Setenv(reexec.InstalledBinEnv, installed)

	calls := captureExec(t)
	resumeID := watchResumeID()
	require.NotEmpty(t, resumeID, "a continuous watch must export a ResumeID for its successor")
	assert.Equal(t, resumeID, watchResumeID(), "the ResumeID must be stable within one watch")

	up, stop := clientWatchForTest(t, installed, func(string) {})
	t.Cleanup(stop)
	require.NotNil(t, up, "a watch launched from a runtime copy gets an upgrade watcher")

	// The upgrade lands: same path, new content.
	require.NoError(t, os.WriteFile(installed, []byte("v2-with-a-longer-body"), 0o755))

	waitFor(t, "the replacement request", up.ReplaceRequested)
	up.Replace()

	got := calls()
	require.Len(t, got, 1, "one change must produce exactly one re-exec")
	assert.Equal(t, installed, got[0].path, "the successor must be the installed binary, not the stale copy")
	assert.Equal(t, os.Args, got[0].argv, "the successor must receive this watch's own command line")
	carried, ok := envValue(got[0].env, watchResumeIDEnv)
	require.True(t, ok, "the successor must inherit the watch ResumeID")
	assert.Equal(t, resumeID, carried, "the successor must resume under the same ID, so the daemon hands the baseline back")
	count, ok := envValue(got[0].env, clientReplaceCountEnv)
	require.True(t, ok, "the successor must learn how many replacements this chain has taken")
	assert.Equal(t, "1", count)
	_, hasDepth := envValue(got[0].env, reexec.ReexecDepthEnv)
	assert.False(t, hasDepth, "the successor starts a fresh re-exec chain")
}

// TestWatchClientQuietWhileUnchanged verifies an unchanged binary produces no
// request and no re-exec: the tick is a stat, not a restart.
func TestWatchClientQuietWhileUnchanged(t *testing.T) {
	t.Setenv(watchResumeIDEnv, "")
	t.Setenv(clientReplaceCountEnv, "")

	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
	t.Setenv(reexec.InstalledBinEnv, installed)

	calls := captureExec(t)
	up, stop := clientWatchForTest(t, installed, func(string) {})
	require.NotNil(t, up)

	time.Sleep(200 * time.Millisecond) // many ticks at the test interval
	assert.False(t, up.ReplaceRequested(), "an unchanged binary must never request a replacement")
	stop()
	assert.Empty(t, calls(), "an unchanged binary must never re-exec")
}

// TestWatchClientReplaceLoopImpossible covers both halves of the loop guard. A
// chain that reaches the cap refuses to replace again, and a replacement that
// fails does not repeat for the same change: the recorded baseline advances, so
// only a NEW change retries. Together they bound how many times one watch chain
// can relaunch, so a successor that dies on start cannot churn forever.
func TestWatchClientReplaceLoopImpossible(t *testing.T) {
	t.Run("cap refuses further replacements", func(t *testing.T) {
		t.Setenv(watchResumeIDEnv, "")
		t.Setenv(clientReplaceCountEnv, strconv.Itoa(maxClientReplacements))

		dir := t.TempDir()
		installed := filepath.Join(dir, "installed-gh-monitor")
		require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
		t.Setenv(reexec.InstalledBinEnv, installed)

		calls := captureExec(t)
		var mu sync.Mutex
		var logs strings.Builder
		record := func(s string) { mu.Lock(); logs.WriteString(s); mu.Unlock() }

		up, stop := clientWatchForTest(t, installed, record)
		t.Cleanup(stop)
		assert.Nil(t, up, "a chain at the cap starts no watcher at all")

		require.NoError(t, os.WriteFile(installed, []byte("v2-bigger"), 0o755))
		time.Sleep(100 * time.Millisecond)
		assert.Empty(t, calls(), "a chain at the cap must not replace itself again")
		mu.Lock()
		joined := logs.String()
		mu.Unlock()
		assert.Contains(t, joined, "replaced", "the refusal must be reported, not silent")
	})

	t.Run("a failed replacement does not repeat for the same change", func(t *testing.T) {
		t.Setenv(watchResumeIDEnv, "")
		t.Setenv(clientReplaceCountEnv, "")

		dir := t.TempDir()
		installed := filepath.Join(dir, "installed-gh-monitor")
		require.NoError(t, os.WriteFile(installed, []byte("v1"), 0o755))
		t.Setenv(reexec.InstalledBinEnv, installed)

		var mu sync.Mutex
		attempts := 0
		orig := execIntoFn
		t.Cleanup(func() { execIntoFn = orig })
		execIntoFn = func(string, []string, []string) error {
			mu.Lock()
			attempts++
			mu.Unlock()
			return os.ErrPermission
		}

		up, stop := clientWatchForTest(t, installed, func(string) {})
		t.Cleanup(stop)
		require.NotNil(t, up)

		require.NoError(t, os.WriteFile(installed, []byte("v2-bigger"), 0o755))
		waitFor(t, "the replacement request", up.ReplaceRequested)
		up.Replace()
		time.Sleep(150 * time.Millisecond) // many further ticks over the same change
		mu.Lock()
		n := attempts
		mu.Unlock()
		assert.Equal(t, 1, n, "a failed exec must not retry until the binary changes again")
		assert.False(t, up.ReplaceRequested(), "the watch must keep serving after a failed exec")
	})
}

// TestReplaceRequiresStableFingerprint pins decision 4: a file that is still
// being written is never executed. The same changed fingerprint must be seen on
// two consecutive ticks before a replacement is requested, and a fingerprint
// that changes again in between restarts that count.
func TestReplaceRequiresStableFingerprint(t *testing.T) {
	u := &clientUpgrader{installed: "/installed", baseline: "v1"}

	assert.False(t, u.observe("v2"), "first sighting of a new fingerprint must only be recorded")
	assert.True(t, u.observe("v2"), "a fingerprint stable across two ticks requests the replacement")

	assert.False(t, u.observe("v3"), "a later change restarts the two-tick count")
	assert.False(t, u.observe("v4"), "a file still being written keeps changing")
	assert.True(t, u.observe("v4"), "once the writes settle, the second sighting requests the replacement")

	assert.False(t, u.observe("v4"), "a fingerprint already acted on must not request twice")
}

// TestUpgradeTargetFallsBackToTheRunningExecutable covers issue #216's second
// permanent case: a daemon launched without the runtime-copy launcher recorded
// no origin, and watching nothing means it can never adopt an upgrade. The file
// it is actually running from is the answer.
func TestUpgradeTargetFallsBackToTheRunningExecutable(t *testing.T) {
	t.Setenv(reexec.InstalledBinEnv, "/some/installed/gh-monitor")
	assert.Equal(t, "/some/installed/gh-monitor", upgradeTarget())

	t.Setenv(reexec.InstalledBinEnv, "")
	exe, err := os.Executable()
	require.NoError(t, err)
	assert.Equal(t, exe, upgradeTarget(), "without a recorded origin, watch the file this process runs from")
}
