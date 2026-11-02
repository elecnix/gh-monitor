//go:build darwin

package ipc

// sunPathLen is sizeof(sun_path) on darwin: 104 bytes, including the NUL.
const sunPathLen = 104
