//go:build windows

package ipc

// processExists is not implemented on Windows, where the daemon socket is not
// handed off either: a refused socket there is read as stale.
func processExists(int) bool { return false }

// withOwnerLock just runs fn: the record is only consulted on Unix.
func withOwnerLock(_ string, fn func() error) error { return fn() }
