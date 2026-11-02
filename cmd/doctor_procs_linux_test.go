//go:build linux

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsGhMonitorWatcher covers the classification the fleet report depends on.
// The daemon case must look at the subcommand position only: a watcher whose
// comment body or flag value happens to be the word "daemon" is still a
// watcher, and hiding it is the invisibility `gh monitor doctor` exists to
// remove.
func TestIsGhMonitorWatcher(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "the daemon", argv: []string{"/usr/local/bin/gh-monitor", "daemon", "--socket", "/run/x.sock"}, want: false},
		{name: "a runtime-copy daemon", argv: []string{"/run/user/1000/gh-monitor/gh-monitor-abc123", "daemon"}, want: false},
		{name: "a watcher", argv: []string{"/usr/local/bin/gh-monitor", "-R", "o/r", "7"}, want: true},
		{name: "a watcher whose comment body is 'daemon'", argv: []string{"/usr/local/bin/gh-monitor", "-R", "o/r", "7", "--body", "daemon"}, want: true},
		{name: "a watcher whose filter is 'daemon'", argv: []string{"/usr/local/bin/gh-monitor", "--pr", "daemon"}, want: true},
		{name: "not gh-monitor", argv: []string{"/usr/bin/gh", "monitor", "-R", "o/r", "7"}, want: false},
		{name: "empty", argv: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isGhMonitorWatcher(tc.argv), "%v", tc.argv)
		})
	}
}
