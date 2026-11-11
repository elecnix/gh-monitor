package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/ipc"
)

// shortSocketPath keeps the path under the ~104-byte sun_path limit macOS
// enforces; t.TempDir() is too long once the file name is added.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gm")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "subdaemon-broker-subscriber.sock")
}

func TestIsSubdaemonSocketNamesTheSupervisedChildsSocket(t *testing.T) {
	if !isSubdaemonSocket("/home/u/.cache/gh-monitor/subdaemon-broker-subscriber.sock") {
		t.Fatal("a subdaemon-* socket must be recognised")
	}
	if isSubdaemonSocket("/home/u/.cache/gh-monitor/daemon.sock") {
		t.Fatal("the daemon's own socket must not be treated as a child's")
	}
}

func TestWaitForFreeSocketBindsAfterThePredecessorReleases(t *testing.T) {
	path := shortSocketPath(t)
	held, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("hold the socket: %v", err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = held.Close()
	}()
	l, err := waitForFreeSocket(context.Background(), path, 2*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("expected the wait to bind once the predecessor released: %v", err)
	}
	_ = l.Close()
}

func TestWaitForFreeSocketGivesUpWhileThePredecessorStays(t *testing.T) {
	path := shortSocketPath(t)
	held, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("hold the socket: %v", err)
	}
	defer held.Close()
	if _, err := waitForFreeSocket(context.Background(), path, 120*time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("a socket held past the grace must return the live-daemon error, not a listener")
	} else if !errors.Is(err, ipc.ErrLiveDaemon) {
		t.Fatalf("expected ErrLiveDaemon, got %v", err)
	}
}
