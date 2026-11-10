//go:build darwin

package orphan

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// procParentOf reads a process's parent pid and command name through the
// kern.proc.pid sysctl. It is the real ParentFunc on macOS. Without it the
// walk could not see past the gh wrapper there, and every watch started
// through the CLI kept polling after its session exited: launchd adopts the
// wrapper, and the watch's own parent never changes.
func procParentOf(pid int) (int, string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, "", false
	}
	comm := kp.Proc.P_comm[:]
	if end := bytes.IndexByte(comm, 0); end >= 0 {
		comm = comm[:end]
	}
	return int(kp.Eproc.Ppid), string(comm), true
}
