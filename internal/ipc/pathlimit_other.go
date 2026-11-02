//go:build !windows && !darwin && !linux

package ipc

// sunPathLen is the smallest of the limits the supported BSDs use, so a path
// this accepts is accepted everywhere gh-monitor runs.
const sunPathLen = 104
