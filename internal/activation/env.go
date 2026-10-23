// Environment shared by every platform. The declarations live here, untagged,
// so the Unix implementation and its no-op sibling cannot drift apart: a field
// added for one is a field both have, and a caller compiles on every platform
// or nowhere.

package activation

import "os"

// Environment variables of the socket-activation protocol.
const (
	// FDsEnv carries how many descriptors were passed.
	FDsEnv = "LISTEN_FDS"
	// PIDEnv names the process they were passed to.
	PIDEnv = "LISTEN_PID"
	// NamesEnv optionally names each descriptor, newline-separated.
	NamesEnv = "LISTEN_FDNAMES"
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
