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
	return syscall.Exec(path, os.Args, os.Environ())
}
