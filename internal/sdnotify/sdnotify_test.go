//go:build unix

package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeManager listens where a service manager would and returns what each
// notification carried.
func fakeManager(t *testing.T) (addr string, recv func() string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sdn-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr = filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return addr, func() string {
		t.Helper()
		buf := make([]byte, 4096)
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		n, err := conn.Read(buf)
		require.NoError(t, err)
		return string(buf[:n])
	}
}

func TestNotify_NoSocketIsANoOp(t *testing.T) {
	t.Setenv(SocketEnv, "")
	assert.False(t, Enabled())
	assert.NoError(t, Ready("serving"), "an unsupervised daemon must not fail to notify nobody")
	assert.NoError(t, MainPID(1))
	assert.NoError(t, Stopping())
}

func TestNotify_SendsEachAssignment(t *testing.T) {
	addr, recv := fakeManager(t)
	t.Setenv(SocketEnv, addr)
	require.True(t, Enabled())

	require.NoError(t, Ready("serving 3 watches"))
	assert.Equal(t, "READY=1\nSTATUS=serving 3 watches", recv())

	require.NoError(t, Ready(""))
	assert.Equal(t, "READY=1", recv())

	require.NoError(t, MainPID(4242))
	assert.Equal(t, "MAINPID=4242", recv())

	require.NoError(t, Stopping())
	assert.Equal(t, "STOPPING=1", recv())
}

func TestNotify_UnreachableSocketIsAnError(t *testing.T) {
	t.Setenv(SocketEnv, filepath.Join(t.TempDir(), "nobody-listens"))
	assert.Error(t, Ready(""), "a supervised daemon must be told its notification was lost")
}

// A sub-daemon must not speak for the unit: READY=1 from one would mark the
// service started before the daemon that owns the socket serves, and MAINPID
// would hand the unit to a process that does not own it.
func TestWithoutNotifyEnv(t *testing.T) {
	env := []string{"PATH=/bin", "NOTIFY_SOCKET=/run/notify", "GH_MONITOR_SOCK=/tmp/x.sock"}
	assert.Equal(t, []string{"PATH=/bin", "GH_MONITOR_SOCK=/tmp/x.sock"}, WithoutNotifyEnv(env))

	keep := []string{"NOTIFY_SOCKET_EXTRA=1"}
	assert.Equal(t, keep, WithoutNotifyEnv(keep), "a name that merely starts with the variable's is not the variable")
}
