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
	"strconv"
	"strings"
)

// firstFD is where the protocol puts the first passed descriptor: right after
// the three standard streams.
const firstFD = 3

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
		// net.FileListener dups the descriptor; f.Close then closes the
		// inherited one, so the process does not keep two per activated
		// socket. It runs before the error check because the descriptor is
		// consumed either way — an adoption that fails must not leak it.
		// (No syscall.CloseOnExec here: it would mark a descriptor that is
		// about to be closed, and it names the number rather than the file,
		// so it would be wrong for any injected File that is a dup.)
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
