//go:build !unix

package activation

import "net"

// Environment variables of the socket-activation protocol. They are declared
// on every platform so callers need no build tags; only Unix ever finds them
// set.
const (
	FDsEnv   = "LISTEN_FDS"
	PIDEnv   = "LISTEN_PID"
	NamesEnv = "LISTEN_FDNAMES"
)

// Env is the socket-activation environment. It exists on every platform so
// callers need no build tags; only Unix can adopt a passed descriptor.
type Env struct{}

// ProcessEnv is the real environment.
func ProcessEnv() Env { return Env{} }

// Listeners reports no passed listeners: socket activation is a Unix facility,
// so a daemon on this platform binds its own socket as it always has.
func Listeners(Env) ([]net.Listener, error) { return nil, nil }
