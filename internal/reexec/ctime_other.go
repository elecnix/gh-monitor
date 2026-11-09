//go:build !linux && !darwin

package reexec

import "os"

// changeTime returns 0: this platform's stat result is not read for an inode
// change time, so ChangeFingerprint falls back to the Fingerprint fields.
func changeTime(os.FileInfo) int64 { return 0 }
