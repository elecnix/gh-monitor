//go:build windows

package ipc

import (
	"errors"
	"syscall"
)

const (
	// processQueryLimitedInformation is the least access that GetExitCodeProcess
	// needs, and is granted for processes of other users too.
	processQueryLimitedInformation = 0x1000
	// stillActive is the exit code GetExitCodeProcess reports for a running process.
	stillActive = 259
)

// withOwnerLock just runs fn: claim and release on Windows are not locked.
func withOwnerLock(_ string, fn func() error) error { return fn() }

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
