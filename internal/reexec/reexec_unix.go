//go:build !windows

package reexec

import (
	"os"
	"syscall"
)

// supported reports whether this platform implements the exec-into-copy
// mechanism. Unix does, via syscall.Exec.
func supported() bool { return true }

// reexecTo replaces the current process with the executable at path, passing
// along the original argv and environment. It only returns on error.
func reexecTo(path string) error {
	return execInto(path, os.Args, os.Environ())
}

// execInto replaces the current process image with path, passing argv and env.
// It only returns on error. The same call serves the runtime-copy relaunch and
// a resident watch replacing itself with an upgraded binary (issue #216).
func execInto(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
