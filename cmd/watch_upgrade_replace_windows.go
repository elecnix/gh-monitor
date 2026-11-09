//go:build windows

package cmd

import "errors"

// replaceProcess is not possible on Windows: the platform has no way to swap a
// running process image while keeping the PID and the parent's pipes. The
// watch logs that it keeps serving its current binary instead of pretending to
// have upgraded (issue #216).
func replaceProcess(string) error {
	return errors.New("a running watch cannot be replaced in place on this platform")
}
