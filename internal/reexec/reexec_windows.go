//go:build windows

package reexec

// supported reports whether this platform implements the exec-into-copy
// mechanism. Windows locks the image of a running executable differently and
// has no exec-without-spawn, so the relaunch is not implemented: resident
// commands run in place, as before issue #73's fix.
func supported() bool { return false }

// reexecTo is unreachable while supported() returns false.
func reexecTo(path string) error { return nil }
