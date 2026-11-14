//go:build linux

package reexec

import "syscall"

// statChangeTime reads the inode change time, in nanoseconds.
func statChangeTime(st *syscall.Stat_t) int64 { return st.Ctim.Nano() }
