//go:build !windows

package cmd

import (
	"os/exec"
	"syscall"
)

// detachProcess configures cmd so the spawned daemon outlives the client.
// On Unix the child becomes its own session leader (setsid), which decouples it
// from the controlling terminal of the parent.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
