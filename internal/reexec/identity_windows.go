//go:build windows

package reexec

import "os"

// identityFields has no inode to report on Windows, where the runtime-copy
// relaunch is not implemented anyway (supported() is false): size and
// modification time identify the file.
func identityFields(os.FileInfo) (inode, device uint64) {
	return 0, 0
}
