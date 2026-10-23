//go:build !linux

package cmd

import "errors"

// listWatchers needs /proc, which this platform does not have. The daemon half
// of `gh monitor doctor` still works; only the fleet comparison is missing, and
// it says so rather than reporting an empty, reassuring fleet.
func listWatchers(int) ([]watcherProcess, error) {
	return nil, errors.New("the process scan needs Linux /proc; only the daemon's own report is available here")
}
