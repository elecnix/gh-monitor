//go:build unix

package activation

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundListener binds a real Unix socket and returns it as an *os.File, the
// shape a passed descriptor arrives in.
func boundListener(t *testing.T) (string, *os.File) {
	t.Helper()
	dir, err := os.MkdirTemp("", "act-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	ul, ok := l.(*net.UnixListener)
	require.True(t, ok)
	// The path must survive closing the listener: adopting it is the point.
	ul.SetUnlinkOnClose(false)
	f, err := ul.File() // a dup: closing it leaves the listener usable
	require.NoError(t, err)
	require.NoError(t, l.Close())
	return path, f
}

// testEnv is the protocol environment as passed() reads it, with File
// injectable so a test needs no real fd 3 — which it cannot safely claim in
// a running test binary.
func testEnv(fds, pid, names string, myPID int, file func(uintptr, string) *os.File, unset *[]string) Env {
	vars := map[string]string{FDsEnv: fds, PIDEnv: pid, NamesEnv: names}
	return Env{
		Lookup: func(k string) string { return vars[k] },
		Unset: func(k string) error {
			if unset != nil {
				*unset = append(*unset, k)
			}
			delete(vars, k)
			return nil
		},
		Pid:  func() int { return myPID },
		File: file,
	}
}

func TestListeners_NotActivatedIsNil(t *testing.T) {
	var unset []string
	l, err := Listeners(testEnv("", "", "", 42, os.NewFile, &unset))
	require.NoError(t, err)
	assert.Nil(t, l)
	assert.Empty(t, unset, "nothing to clear when nothing was passed")
}

func TestListeners_AdoptsPassedSocketAndClearsEnv(t *testing.T) {
	path, f := boundListener(t)
	t.Cleanup(func() { _ = f.Close() })

	var unset []string
	ls, err := Listeners(testEnv("1", "42", "gh-monitor.sock", 42,
		func(uintptr, string) *os.File { return f }, &unset))
	require.NoError(t, err)
	require.Len(t, ls, 1)
	t.Cleanup(func() { _ = ls[0].Close() })
	assert.ElementsMatch(t, []string{FDsEnv, PIDEnv, NamesEnv}, unset,
		"every protocol variable must be cleared so no child inherits it")

	// The adopted listener really is the passed socket: a client that dials
	// the path reaches it.
	c, err := net.Dial("unix", path)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	accepted, err := ls[0].Accept()
	require.NoError(t, err)
	_ = accepted.Close()
}

func TestListeners_IgnoresDescriptorsAddressedToAnotherPID(t *testing.T) {
	path, f := boundListener(t)
	t.Cleanup(func() { _ = f.Close() })

	var unset []string
	called := false
	ls, err := Listeners(testEnv("1", "7", "", 42,
		func(uintptr, string) *os.File { called = true; return f }, &unset))
	require.NoError(t, err)
	assert.Nil(t, ls, "a count addressed to another pid is not ours to adopt")
	assert.Empty(t, unset, "and its variables must be left alone for that process")
	assert.False(t, called, "no descriptor may be claimed on the way to that decision")
	assert.FileExists(t, path, "the socket belongs to the process it was passed to")
}

func TestListeners_UnparseableOrZeroCountIsNotActivation(t *testing.T) {
	for _, v := range []string{"zero", "-1", "0"} {
		ls, err := Listeners(testEnv(v, "", "", 42, os.NewFile, nil))
		require.NoError(t, err, v)
		assert.Nil(t, ls, v)
	}
}

func TestListeners_NonListenerDescriptorIsAnError(t *testing.T) {
	// A pipe is not a listener. Returning nil here would let the daemon bind
	// its own socket while the passed one queues connections forever.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	t.Cleanup(func() { _ = w.Close() })

	ls, err := Listeners(testEnv("1", "", "", 42,
		func(uintptr, string) *os.File { return r }, nil))
	require.Error(t, err, "a passed descriptor that is not a listener must not be silently skipped")
	assert.Nil(t, ls)
}

// TestListeners_ClosesTheInheritedDescriptor pins the thing a review called a
// leak: net.FileListener dups the descriptor, and the inherited one must be
// closed or every activated socket costs two. The check is EBADF on the
// descriptor afterwards, which is the only evidence that nothing holds it.
func TestListeners_ClosesTheInheritedDescriptor(t *testing.T) {
	path, f := boundListener(t)
	t.Cleanup(func() { _ = f.Close() })
	// A dup of the bound socket, standing in for the descriptor systemd
	// passes; the test's own handle stays valid so it can be compared against.
	inherited, err := syscall.Dup(int(f.Fd()))
	require.NoError(t, err)
	passed := os.NewFile(uintptr(inherited), "passed")

	ls, err := Listeners(testEnv("1", "", "", os.Getpid(),
		func(uintptr, string) *os.File { return passed }, nil))
	require.NoError(t, err)
	require.Len(t, ls, 1)
	t.Cleanup(func() { _ = ls[0].Close() })

	// The inherited descriptor is gone...
	assert.ErrorIs(t, syscall.Fstat(inherited, &syscall.Stat_t{}), syscall.EBADF,
		"the inherited descriptor must be closed, not merely marked close-on-exec")
	// ...while the adopted listener still serves the same path.
	c, err := net.Dial("unix", path)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	accepted, err := ls[0].Accept()
	require.NoError(t, err)
	_ = accepted.Close()
}

// TestListeners_ClosesTheDescriptorWhenAdoptionFails is the same guarantee on
// the error path: a descriptor that cannot be adopted is still consumed, and
// leaking it would make a misconfigured unit leak one per restart.
func TestListeners_ClosesTheDescriptorWhenAdoptionFails(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	t.Cleanup(func() { _ = w.Close() })

	inherited, err := syscall.Dup(int(r.Fd()))
	require.NoError(t, err)
	passed := os.NewFile(uintptr(inherited), "passed")

	_, err = Listeners(testEnv("1", "", "", os.Getpid(),
		func(uintptr, string) *os.File { return passed }, nil))
	require.Error(t, err, "a pipe is not a listener")
	assert.ErrorIs(t, syscall.Fstat(inherited, &syscall.Stat_t{}), syscall.EBADF,
		"an adopted-but-rejected descriptor must not be leaked either")
}
