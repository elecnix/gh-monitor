//go:build windows

package sdnotify

// post is unreachable in practice — Windows has no sd_notify, so nothing sets
// $NOTIFY_SOCKET and Enabled() reports false. The stub exists so the package
// builds for the Windows release target.
func post(string, []byte) error { return nil }
