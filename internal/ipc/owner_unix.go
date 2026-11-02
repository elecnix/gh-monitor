//go:build !windows

package ipc

import (
	"errors"
	"syscall"
)

// processExists reports whether pid names a process. EPERM means it exists but
// belongs to someone else.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
