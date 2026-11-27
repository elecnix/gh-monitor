//go:build !windows

package reexec

import (
	"os"
	"syscall"
)

// identityFields exposes the stat fields that make a file version specific.
// The inode changes on any atomic replace, so a copy name derived from it can
// never be shared by two different builds. A write sets the inode change time
// to the filesystem's current time, which is what lets ChangeFingerprint see
// an in-place rewrite, unless the rewrite lands in the same timestamp tick as
// the previous change (issue #237).
func identityFields(fi os.FileInfo) (inode, device uint64, ctime int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, 0
	}
	return uint64(st.Ino), uint64(st.Dev), statChangeTime(st)
}
