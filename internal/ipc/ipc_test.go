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
	dir, err := os.MkdirTemp(shortTmp(), "ghmon-listen-*.d")
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
	dir, err := os.MkdirTemp(shortTmp(), "ghmon-stale-*.d")
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
	dir, err := os.MkdirTemp(shortTmp(), "ghmon-wait-*.d")
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

// shortTmp is where socket fixtures create their directory: darwin caps a Unix
// socket path at 104 bytes and TMPDIR there is a long /var/folders/... path,
// so a socket built on t.TempDir() cannot be bound at all.
func shortTmp() string {
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		return "/tmp"
	}
	return os.TempDir()
}

// staleSocketFile leaves a socket file that refuses every connection: bound,
// listening, then closed without unlinking. A daemon whose accept queue is
// full refuses connections the same way on macOS, so the dial alone cannot
// tell the two apart.
func staleSocketFile(t *testing.T, sock string) {
	t.Helper()
	l, err := net.Listen("unix", sock)
	require.NoError(t, err)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, l.Close())
	_, err = os.Stat(sock)
	require.NoError(t, err)
}

// TestListen_KeepsARefusedSocketWhoseOwnerIsAlive is the guard against the
// successor that steals the path from a daemon that is alive but not
// accepting. The old daemon keeps running on an orphaned socket and every
// client that reaches it sees a hello and then a broken pipe.
func TestListen_KeepsARefusedSocketWhoseOwnerIsAlive(t *testing.T) {
	dir, err := os.MkdirTemp(shortTmp(), "ghmon-owner-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	staleSocketFile(t, sock)
	// The parent process stands in for a live daemon: any process but this one.
	require.NoError(t, os.WriteFile(OwnerPath(sock), []byte(strconv.Itoa(os.Getppid())), 0o600))

	_, err = Listen(sock)
	require.ErrorIs(t, err, ErrLiveDaemon)
	_, statErr := os.Stat(sock)
	assert.NoError(t, statErr, "a socket with a live owner must not be unlinked")
}

// TestListen_ReclaimsARefusedSocketWhoseOwnerIsGone keeps the crash case
// working: the owner record names a process that no longer exists.
func TestListen_ReclaimsARefusedSocketWhoseOwnerIsGone(t *testing.T) {
	dir, err := os.MkdirTemp(shortTmp(), "ghmon-gone-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	staleSocketFile(t, sock)
	require.NoError(t, os.WriteFile(OwnerPath(sock), []byte("2147483646"), 0o600))

	l, err := Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
}

// TestListen_RecordsItsOwner lets the next starter tell a wedged daemon from a
// dead one, and ReleaseOwner leaves a successor's record alone.
func TestListen_RecordsItsOwner(t *testing.T) {
	dir, err := os.MkdirTemp(shortTmp(), "ghmon-rec-*.d")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	l, err := Listen(sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	raw, err := os.ReadFile(OwnerPath(sock))
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(os.Getpid()), strings.TrimSpace(string(raw)))

	require.NoError(t, os.WriteFile(OwnerPath(sock), []byte("2147483646"), 0o600))
	ReleaseOwner(sock)
	_, err = os.Stat(OwnerPath(sock))
	assert.NoError(t, err, "ReleaseOwner must not remove a record that names another process")

	require.NoError(t, ClaimOwner(sock))
	ReleaseOwner(sock)
	_, err = os.Stat(OwnerPath(sock))
	assert.True(t, os.IsNotExist(err))
}

// TestListen_ReadsAMalformedOwnerRecordAsNoOwner covers pids that signal
// process groups instead of naming a process.
func TestListen_ReadsAMalformedOwnerRecordAsNoOwner(t *testing.T) {
	for _, record := range []string{"0", "-1", "", "abc"} {
		dir, err := os.MkdirTemp(shortTmp(), "ghmon-bad-*.d")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "d.sock")
		staleSocketFile(t, sock)
		require.NoError(t, os.WriteFile(OwnerPath(sock), []byte(record), 0o600))

		l, err := Listen(sock)
		require.NoError(t, err, "record %q", record)
		_ = l.Close()
	}
}
