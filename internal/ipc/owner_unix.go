//go:build !windows

package ipc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// withOwnerLock runs fn holding an exclusive lock beside the owner record, so
// a claim and a release cannot interleave. The lock file is never removed:
// removing it would let two processes lock different files.
func withOwnerLock(socket string, fn func() error) error {
	f, err := os.OpenFile(OwnerPath(socket)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock the owner record of %s: %w", socket, err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock the owner record of %s: %w", socket, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// processExists reports whether pid names a process. EPERM means it exists but
// belongs to someone else.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
