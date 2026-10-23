//go:build unix

// Package activation adopts a listening socket that a service manager passed
// to this process (systemd's socket activation), so the socket outlives every
// daemon that serves on it (issue #193).
//
// Without it, a restart — supervised or not — has to move the listening
// socket from the outgoing daemon to its successor by hand (the SCM_RIGHTS
// pass in internal/handoff), and loses it entirely if the outgoing daemon
// dies. With it, systemd holds the socket for the whole session: every start
// of the service receives the same fd, a restart is invisible to a client's
// connect(), and the handoff only has to carry the watching state.
//
// The protocol is three environment variables, and its own rules are strict
// about them: LISTEN_FDS is a count, LISTEN_PID names the process they were
// passed to, and both are inherited across exec — so a process that adopts
// must unset them, or a child it spawns would try to adopt descriptors that
// describe nothing.
package activation

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Environment variables of the socket-activation protocol.
const (
	// FDsEnv carries how many descriptors were passed.
	FDsEnv = "LISTEN_FDS"
	// PIDEnv names the process they were passed to.
	PIDEnv = "LISTEN_PID"
	// NamesEnv optionally names each descriptor, newline-separated.
	NamesEnv = "LISTEN_FDNAMES"
	// firstFD is where the protocol puts the first passed descriptor: right
	// after the three standard streams.
	firstFD = 3
)

// Env is the environment as seen by the adopting process. Tests substitute
// their own so a passed descriptor can be simulated without a service
// manager.
type Env struct {
	// Lookup returns the value of an environment variable, like os.Getenv.
	Lookup func(string) string
	// Unset removes a variable, like os.Unsetenv.
	Unset func(string) error
	// Pid returns this process's pid, like os.Getpid.
	Pid func() int
	// File wraps a passed descriptor, like os.NewFile.
	File func(fd uintptr, name string) *os.File
}

// ProcessEnv is the real environment.
func ProcessEnv() Env {
	return Env{Lookup: os.Getenv, Unset: os.Unsetenv, Pid: os.Getpid, File: os.NewFile}
}

// Listeners returns the descriptors the service manager passed to this
// process as listeners, and clears the protocol's environment so no child
// inherits it. It returns nil when this process was not socket-activated.
//
// A passed descriptor that cannot be wrapped as a listener is an error, not
// something to skip: a daemon would otherwise bind its own socket and serve
// two listeners' worth of clients from one of them, with the other one's
// connections queued forever.
func Listeners(env Env) ([]net.Listener, error) {
	n, names, ok := passed(env)
	if !ok {
		return nil, nil
	}
	// Clear first, so every exit path below leaves a clean environment: the
	// descriptors are consumed exactly once, here.
	_ = env.Unset(FDsEnv)
	_ = env.Unset(PIDEnv)
	_ = env.Unset(NamesEnv)

	out := make([]net.Listener, 0, n)
	for i := 0; i < n; i++ {
		fd := uintptr(firstFD + i)
		name := ""
		if i < len(names) {
			name = names[i]
		}
		f := env.File(fd, name)
		if f == nil {
			return nil, fmt.Errorf("activation: no descriptor %d", fd)
		}
		// syscall.CloseOnExec, not f.Close: net.FileListener dups the
		// descriptor, so the inherited one must be closed without touching
		// the copy the listener now owns.
		syscall.CloseOnExec(int(fd))
		l, err := net.FileListener(f)
		closeErr := f.Close()
		if err != nil {
			return nil, fmt.Errorf("activation: descriptor %d is not a listener: %w", fd, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("activation: descriptor %d: %w", fd, closeErr)
		}
		out = append(out, l)
	}
	return out, nil
}

// passed reports how many descriptors apply to this process, and their
// optional names. A count addressed to another pid is not ours: the
// variables survive exec, so a child must ignore what its parent was given.
func passed(env Env) (int, []string, bool) {
	raw := env.Lookup(FDsEnv)
	if raw == "" {
		return 0, nil, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, nil, false
	}
	if pidRaw := env.Lookup(PIDEnv); pidRaw != "" {
		pid, perr := strconv.Atoi(pidRaw)
		if perr != nil || (env.Pid != nil && pid != env.Pid()) {
			return 0, nil, false
		}
	}
	var names []string
	if rawNames := env.Lookup(NamesEnv); rawNames != "" {
		names = strings.Split(rawNames, ":")
	}
	return n, names, true
}
