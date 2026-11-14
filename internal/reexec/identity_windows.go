//go:build windows

package reexec

import "os"

// identityFields has no inode or change time to report on Windows, where the
// runtime-copy relaunch is not implemented anyway (supported() is false):
// size and modification time identify the file.
func identityFields(os.FileInfo) (inode, device uint64, ctime int64) {
	return 0, 0, 0
}
