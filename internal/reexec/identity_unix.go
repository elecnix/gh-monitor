//go:build !windows

package reexec

import (
	"os"
	"syscall"
)

// identityFields exposes the stat fields that make a file version specific.
// The inode changes on any atomic replace, so a copy name derived from it can
// never be shared by two different builds. The inode change time moves on any
// write, which is what lets ChangeFingerprint see an in-place rewrite.
func identityFields(fi os.FileInfo) (inode, device uint64, ctime int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, 0
	}
	return uint64(st.Ino), uint64(st.Dev), statChangeTime(st)
}
