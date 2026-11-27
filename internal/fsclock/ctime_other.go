//go:build !linux && !darwin

package fsclock

import (
	"os"
	"time"
)

// changeTime returns the zero time: this platform's stat result is not read
// for an inode change time, so the wait relies on mtime alone.
func changeTime(os.FileInfo) time.Time { return time.Time{} }
