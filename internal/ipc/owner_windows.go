//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	// processQueryLimitedInformation is the least access that GetExitCodeProcess
	// needs, and is granted for processes of other users too.
	processQueryLimitedInformation = 0x1000
	// stillActive is the exit code GetExitCodeProcess reports for a running process.
	stillActive = 259
)

// lockfileExclusiveLock is LOCKFILE_EXCLUSIVE_LOCK for LockFileEx.
const lockfileExclusiveLock = 2

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// withOwnerLock runs fn holding an exclusive lock beside the owner record, so
// a claim and a release cannot interleave. The lock file is never removed:
// removing it would let two processes lock different files. Closing the file
// releases the lock even if the process dies holding it.
func withOwnerLock(socket string, fn func() error) error {
	f, err := os.OpenFile(OwnerPath(socket)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock the owner record of %s: %w", socket, err)
	}
	defer func() { _ = f.Close() }()
	h := f.Fd()
	var ol syscall.Overlapped
	// Lock one byte, blocking until it is free.
	if r, _, e := procLockFileEx.Call(h, lockfileExclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(&ol))); r == 0 {
		return fmt.Errorf("lock the owner record of %s: %w", socket, e)
	}
	defer func() { _, _, _ = procUnlockFileEx.Call(h, 0, 1, 0, uintptr(unsafe.Pointer(&ol))) }()
	return fn()
}

// processExists reports whether pid names a running process. A process that
// denies the query still exists.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
