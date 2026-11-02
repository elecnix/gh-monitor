package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListen_RejectsLiveSocket is the race-safety guarantee that makes
// concurrent autostart spawners safe: if a daemon is already bound to the
// socket, a second Listen must fail (not os.Remove the live socket and steal
// the path), so at most one daemon ever serves a given socket.
func TestListen_RejectsLiveSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "ghmon-listen-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	// First listener binds the socket — a live daemon.
	first, err := Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })

	// A second Listen on the same path must refuse, because the socket is live.
	_, err = Listen(sock)
	assert.Error(t, err, "a second Listen must not steal a live daemon's socket")

	// The first daemon is still serving — the second Listen did not clobber it.
	c, err := net.Dial("unix", sock)
	require.NoError(t, err)
	_ = c.Close()
}

// TestListen_ReclaimsStaleSocket verifies a stale socket (no live daemon) is
// removed and reused, so a crashed daemon does not block the next start.
func TestListen_ReclaimsStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "ghmon-stale-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	// Leave a stale socket file with no listener behind.
	require.NoError(t, os.WriteFile(sock, []byte("stale"), 0o600))

	l, err := Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	// The new listener accepts connections.
	c, err := net.Dial("unix", sock)
	require.NoError(t, err)
	_ = c.Close()
}

// TestWaitReady_Timeout verifies WaitReady gives up when nothing binds.
func TestWaitReady_Timeout(t *testing.T) {
	dir, err := os.MkdirTemp("", "ghmon-wait-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "never.sock")

	err = WaitReady(context.Background(), sock, 300*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "did not come up")
}

// TestListen_RejectsAPathOverThePlatformLimit is the failure a real machine
// produces with a long user name or a deep $XDG_RUNTIME_DIR: bind answers
// EINVAL, which says nothing about length. The check names the length, the
// limit and the path instead.
func TestListen_RejectsAPathOverThePlatformLimit(t *testing.T) {
	dir := t.TempDir()
	long := filepath.Join(dir, strings.Repeat("d", maxPathLen()))
	_, err := Listen(long)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "socket path is")
	assert.Contains(t, err.Error(), strconv.Itoa(maxPathLen()), "the message must name the limit")
	assert.Contains(t, err.Error(), "GH_MONITOR_SOCK", "and the way out of it")
}

// TestListen_AcceptsAPathUnderTheLimit keeps the guard from being stricter than
// the kernel: a path the platform can bind must still bind.
func TestListen_AcceptsAPathUnderTheLimit(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ghm-ipc-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	require.Less(t, len(path), maxPathLen())

	l, err := Listen(path)
	require.NoError(t, err)
	_ = l.Close()
}
