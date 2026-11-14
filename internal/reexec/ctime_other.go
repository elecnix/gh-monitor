//go:build !linux && !darwin && !windows

package reexec

import "syscall"

// statChangeTime returns 0: this platform's stat result is not read for an
// inode change time, so ChangeFingerprint falls back to the Fingerprint fields.
func statChangeTime(*syscall.Stat_t) int64 { return 0 }
