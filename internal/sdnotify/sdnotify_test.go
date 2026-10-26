package sdnotify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listenManager stands up a datagram socket where a service manager's
// notification socket would be, and returns a channel of the messages it
// receives. Each message is one datagram, exactly as systemd reads them.
func listenManager(t *testing.T, name string) (*net.UnixConn, <-chan string) {
	t.Helper()
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on %s: %v", name, err)
	}
	t.Cleanup(func() { _ = l.Close() })

	msgs := make(chan string, 8)
	go func() {
		defer close(msgs)
		buf := make([]byte, 4096)
		for {
			n, err := l.Read(buf)
			if err != nil {
				return
			}
			msgs <- string(buf[:n])
		}
	}()
	return l, msgs
}

// expect reads one datagram, or fails the test rather than hanging forever on
// a notification that never arrived.
func expect(t *testing.T, msgs <-chan string) string {
	t.Helper()
	select {
	case m, ok := <-msgs:
		if !ok {
			t.Fatal("manager socket closed before a message arrived")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no notification reached the manager socket")
		return ""
	}
}

// deliver runs one notification against a manager socket named the way the
// service manager names it — a filesystem path, or an abstract-namespace name
// spelled with a leading '@' — and returns the single datagram it received.
func deliver(t *testing.T, socketName string, notify func() error) string {
	t.Helper()
	_, msgs := listenManager(t, socketName)
	t.Setenv(NotifySocketEnv, socketName)
	if err := notify(); err != nil {
		t.Fatalf("notify over %s: %v", socketName, err)
	}
	return expect(t, msgs)
}

// TestNotificationsReachTheManager covers every assignment the daemon sends,
// over both spellings of the manager's socket. The abstract case is the one
// that would otherwise fail silently: a leading '@' is what systemd writes,
// while the kernel wants a leading NUL, and a missed translation connects to
// nothing at all — the manager simply waits forever, with no error anywhere.
func TestNotificationsReachTheManager(t *testing.T) {
	cases := []struct {
		name   string
		notify func() error
		want   string
	}{
		{"ready", Ready, "READY=1"},
		{"main pid", func() error { return MainPID(4242) }, "MAINPID=4242"},
		{"stopping", Stopping, "STOPPING=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" over a path socket", func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "notify.sock")
			if got := deliver(t, sock, tc.notify); got != tc.want {
				t.Fatalf("payload = %q, want %q", got, tc.want)
			}
		})
		t.Run(tc.name+" over an abstract socket", func(t *testing.T) {
			name := fmt.Sprintf("@ghmon-notify-%d-%s", os.Getpid(), strings.ReplaceAll(tc.name, " ", "-"))
			if got := deliver(t, name, tc.notify); got != tc.want {
				t.Fatalf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSendIsInertWithoutManager pins that an unsupervised process pays nothing
// and reports no error — being unsupervised is normal.
func TestSendIsInertWithoutManager(t *testing.T) {
	t.Setenv(NotifySocketEnv, "")
	if Enabled() {
		t.Fatal("Enabled() must be false with no manager")
	}
	for _, fn := range []func() error{Ready, Stopping, func() error { return MainPID(1) }} {
		if err := fn(); err != nil {
			t.Fatalf("notifying with no manager must be a no-op, got %v", err)
		}
	}
}

// TestSendReportsAMissingSocket verifies the only failure the protocol can
// observe is reported rather than swallowed: a manager that named a socket
// nobody is listening on.
func TestSendReportsAMissingSocket(t *testing.T) {
	t.Setenv(NotifySocketEnv, filepath.Join(t.TempDir(), "absent.sock"))
	if err := Ready(); err == nil {
		t.Fatal("Ready must report a socket it could not write to")
	}
}

// TestWithoutNotifyEnv covers the sub-daemon case: a child must not be able to
// mark the unit ready, or to hand the unit to a process that does not own the
// socket.
func TestWithoutNotifyEnv(t *testing.T) {
	env := []string{"PATH=/bin", "NOTIFY_SOCKET=/run/notify", "GH_MONITOR_SOCK=/tmp/x.sock"}
	got := WithoutNotifyEnv(env)
	want := []string{"PATH=/bin", "GH_MONITOR_SOCK=/tmp/x.sock"}
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("env[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// A prefix that merely starts with the variable name is not the variable.
	keep := []string{"NOTIFY_SOCKET_EXTRA=1"}
	if got := WithoutNotifyEnv(keep); len(got) != 1 || got[0] != keep[0] {
		t.Fatalf("WithoutNotifyEnv dropped an unrelated variable: %v", got)
	}
}
