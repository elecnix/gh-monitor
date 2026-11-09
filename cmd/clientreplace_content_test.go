package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/reexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runningImageForTest points the upgrader at a stand-in for the image this
// process runs, so a test decides whether the candidate's content matches it.
func runningImageForTest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh-monitor-running")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	orig := runningImageFn
	runningImageFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { runningImageFn = orig })
	return path
}

// installedForTest writes the installed binary and records it as the watch's
// origin.
func installedForTest(t *testing.T, content string) string {
	t.Helper()
	t.Setenv(watchResumeIDEnv, "")
	t.Setenv(clientReplaceCountEnv, "")
	installed := filepath.Join(t.TempDir(), "installed-gh-monitor")
	require.NoError(t, os.WriteFile(installed, []byte(content), 0o755))
	t.Setenv(reexec.InstalledBinEnv, installed)
	return installed
}

// TestWatchClientReplacesOnContentChangeWithSameStat covers the rewrite the
// stat fingerprint alone misses: the same file rewritten in place with the
// same size, and its mtime set back. Device, inode, size and mtime all match
// the baseline. The inode change time still moves, and the content check then
// confirms the bytes differ.
func TestWatchClientReplacesOnContentChangeWithSameStat(t *testing.T) {
	installed := installedForTest(t, "v1-body")
	runningImageForTest(t, "v1-body")
	calls := captureExec(t)

	info, err := os.Stat(installed)
	require.NoError(t, err)

	up, stop := clientWatchForTest(t, installed, func(string) {})
	t.Cleanup(stop)
	require.NotNil(t, up)

	// Same inode, same length, same mtime, different content.
	f, err := os.OpenFile(installed, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("v2-body"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Chtimes(installed, info.ModTime(), info.ModTime()))
	after, err := os.Stat(installed)
	require.NoError(t, err)
	require.True(t, os.SameFile(info, after), "the rewrite must keep the inode")
	require.Equal(t, info.ModTime(), after.ModTime())

	waitFor(t, "the replacement request", up.ReplaceRequested)
	up.Replace(time.Time{})
	assert.Len(t, calls(), 1, "different content must replace the watch")
}

// TestWatchClientIgnoresAStatChangeWithSameContent pins the other side: a stat
// change whose bytes equal the running image is not an upgrade, so `touch` or
// a reinstall of the same build never relaunches a watch.
func TestWatchClientIgnoresAStatChangeWithSameContent(t *testing.T) {
	installed := installedForTest(t, "same-build")
	runningImageForTest(t, "same-build")
	calls := captureExec(t)

	var mu sync.Mutex
	var logs strings.Builder
	up, stop := clientWatchForTest(t, installed, func(s string) { mu.Lock(); logs.WriteString(s); mu.Unlock() })
	require.NotNil(t, up)

	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(installed, later, later))

	waitFor(t, "the same-content notice", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logs.String(), "same content")
	})
	time.Sleep(100 * time.Millisecond) // further ticks over the same metadata
	assert.False(t, up.ReplaceRequested(), "identical content must not request a replacement")
	stop()
	assert.Empty(t, calls(), "identical content must never re-exec")
}

// TestWatchClientHashFailureKeepsServing pins that a read failure is never
// mistaken for "same content" or for "different content": the watch keeps
// serving, and a later tick that can read the file decides.
func TestWatchClientHashFailureKeepsServing(t *testing.T) {
	installed := installedForTest(t, "v1")
	runningImageForTest(t, "v1")
	calls := captureExec(t)

	var mu sync.Mutex
	failing := true
	orig := binarySumFn
	t.Cleanup(func() { binarySumFn = orig })
	binarySumFn = func(path string) (string, error) {
		mu.Lock()
		f := failing && path == installed
		mu.Unlock()
		if f {
			return "", errors.New("read interrupted")
		}
		return orig(path)
	}

	up, stop := clientWatchForTest(t, installed, func(string) {})
	t.Cleanup(stop)
	require.NotNil(t, up)

	require.NoError(t, os.WriteFile(installed, []byte("v2-with-a-longer-body"), 0o755))
	time.Sleep(150 * time.Millisecond) // many ticks over a settled, unreadable candidate
	assert.False(t, up.ReplaceRequested(), "a hash failure must not request a replacement")
	assert.Empty(t, calls())

	mu.Lock()
	failing = false
	mu.Unlock()
	waitFor(t, "the replacement once the candidate reads", up.ReplaceRequested)
}

// TestRepoWatchWithoutInstanceKeepsItsBinary pins idea 2 of issue #216: a repo
// watch without --instance keeps its cursor in memory only, so a replacement
// would replay events or skip them. It keeps its binary and says so once. The
// same watch with --instance stores its cursor and replaces itself.
func TestRepoWatchWithoutInstanceKeepsItsBinary(t *testing.T) {
	t.Run("no instance", func(t *testing.T) {
		installedForTest(t, "v1")
		var logs strings.Builder
		up := startWatchUpgrade(t.Context(), writerFunc(func(s string) { logs.WriteString(s) }), backend.KindRepo, "")
		assert.Nil(t, up, "a repo watch without --instance must not watch for upgrades")
		assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "the skip is reported on one line")
		assert.Contains(t, logs.String(), "--instance")
	})

	t.Run("with instance", func(t *testing.T) {
		installed := installedForTest(t, "v1")
		calls := captureExec(t)
		origInterval := upgradeCheckInterval
		upgradeCheckInterval = 10 * time.Millisecond
		t.Cleanup(func() { upgradeCheckInterval = origInterval })

		up := startWatchUpgrade(t.Context(), writerFunc(func(string) {}), backend.KindRepo, "ci")
		require.NotNil(t, up, "a repo watch with --instance stores its cursor, so it can replace itself")
		require.NoError(t, os.WriteFile(installed, []byte("v2-with-a-longer-body"), 0o755))
		waitFor(t, "the replacement request", up.ReplaceRequested)
		up.Replace(time.Time{})
		assert.Len(t, calls(), 1)
	})

	t.Run("pr watch without instance", func(t *testing.T) {
		installedForTest(t, "v1")
		up := startWatchUpgrade(t.Context(), writerFunc(func(string) {}), backend.KindPR, "")
		assert.NotNil(t, up, "a pr watch resumes through its ResumeID, so it needs no instance")
	})
}
