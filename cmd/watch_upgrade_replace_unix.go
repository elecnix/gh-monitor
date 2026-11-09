//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// replaceProcess replaces this process image with the installed binary,
// keeping argv and environment.
//
// An in-place exec, not a spawn: the PID survives, so the session's pipes stay
// connected, nothing has to be reparented, and the operator sees a watch that
// never stopped. The installed file is only exec'd for an instant — the
// runtime-copy launcher in internal/reexec immediately copies it to a fresh
// runtime path and execs that — so the running image never maps the file the
// next upgrade has to rewrite.
func replaceProcess(path string) error {
	return syscall.Exec(path, os.Args, os.Environ())
}
