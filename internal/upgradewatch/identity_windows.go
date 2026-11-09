//go:build windows

package upgradewatch

import "os"

// fileIdentity is unavailable on Windows: there is no stable inode to compare,
// so size and mtime carry the cheap pre-test and the content hash decides.
func fileIdentity(os.FileInfo) (dev, ino uint64) { return 0, 0 }
