//go:build darwin

package reexec

import (
	"os"
	"syscall"
)

// changeTime returns the inode change time in nanoseconds, or 0 when the
// platform's stat result carries none.
func changeTime(fi os.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0
	}
	return st.Ctimespec.Nano()
}
