//go:build !linux

package sdnotify

// Supervised reports whether this process runs inside a service manager's
// supervision. Only Linux exposes that as a cgroup leaf cheaply, and only
// systemd speaks this protocol, so elsewhere the answer is no: a daemon then
// stays silent, exactly as it did before issue #193.
func Supervised() bool { return false }
