//go:build !windows

package upgradewatch

import (
	"os"
	"syscall"
)

// fileIdentity extracts the device and inode that, together with size and
// mtime, identify one version of the file on this platform.
func fileIdentity(info os.FileInfo) (dev, ino uint64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino)
}
