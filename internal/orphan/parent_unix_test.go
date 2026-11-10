//go:build linux || darwin

package orphan

import (
	"os"
	"testing"
)

// TestProcParentOfReadsThisProcess pins the platform reader: it must report
// this process's parent as the OS does, or the launcher walk cannot see past
// a wrapper on this platform and every watch started through gh leaks.
func TestProcParentOfReadsThisProcess(t *testing.T) {
	ppid, comm, ok := procParentOf(os.Getpid())
	if !ok {
		t.Fatal("procParentOf could not inspect this process")
	}
	if ppid != os.Getppid() {
		t.Fatalf("procParentOf ppid = %d, want %d", ppid, os.Getppid())
	}
	if comm == "" {
		t.Fatal("procParentOf returned an empty command name")
	}
}
