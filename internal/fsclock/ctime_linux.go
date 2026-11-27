package fsclock

import (
	"os"
	"syscall"
	"time"
)

// changeTime reads the inode change time. It returns the zero time when the
// stat result is not a syscall.Stat_t.
func changeTime(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Unix())
}
