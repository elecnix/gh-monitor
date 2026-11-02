//go:build !windows

package ipc

// maxPathLen is the longest path a Unix socket can be bound to on this
// platform. The kernel stores it in a fixed-size sun_path (104 bytes on
// darwin, 108 on Linux), and bind answers EINVAL — "invalid argument" — when it
// does not fit, which names neither the length nor the limit.
func maxPathLen() int { return sunPathLen }
