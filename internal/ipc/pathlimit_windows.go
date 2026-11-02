//go:build windows

package ipc

// maxPathLen is not a meaningful limit for the Windows named-pipe transport,
// so it is large enough never to refuse a path here.
func maxPathLen() int { return 1 << 20 }
