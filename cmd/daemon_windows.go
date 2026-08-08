//go:build windows

package cmd

import (
	"os/exec"
	"syscall"
)

// Windows creation flags for a detached, console-less process that survives
// the parent exiting.
const (
	detachedProcess = 0x00000008 // DETACHED_PROCESS (not exported by syscall)
)

// detachProcess configures cmd so the spawned daemon outlives the client.
// On Windows the child is created in its own process group and detached from
// any console, which is the closest equivalent to setsid.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
}
