//go:build linux

package ipc

// sunPathLen is sizeof(sun_path) on Linux: 108 bytes, including the NUL.
const sunPathLen = 108
