//go:build linux

package sdnotify

import "testing"

// TestSupervisedByCgroup covers the shapes /proc/self/cgroup actually has, and
// the distinction the handoff depends on: a service unit supervises its
// process, a session scope does not.
func TestSupervisedByCgroup(t *testing.T) {
	cases := []struct {
		name string
		file string
		want bool
	}{
		{
			name: "cgroup v2 service",
			file: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/gh-monitor-daemon.service\n",
			want: true,
		},
		{
			name: "cgroup v1 service, one line per controller",
			file: "12:pids:/user.slice/user-1000.slice/user@1000.service/gh-monitor-daemon.service\n" +
				"1:name=systemd:/user.slice/user-1000.slice/user@1000.service/gh-monitor-daemon.service\n",
			want: true,
		},
		{
			// The reload case: `gh monitor reload` spawns its successor from the
			// operator's shell, and that process is in a session scope, not in the
			// unit. It must not claim the unit.
			name: "session scope",
			file: "0::/user.slice/user-1000.slice/user@1000.service/session.slice/session-3.scope\n",
			want: false,
		},
		{
			name: "root cgroup",
			file: "0::/\n",
			want: false,
		},
		{
			name: "empty or unreadable",
			file: "",
			want: false,
		},
		{
			name: "malformed line is skipped, not fatal",
			file: "not-a-cgroup-line\n0::/user.slice/gh-monitor-daemon.service\n",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisedByCgroup(tc.file); got != tc.want {
				t.Fatalf("supervisedByCgroup(%q) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}
