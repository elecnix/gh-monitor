//go:build linux

package orphan

import (
	"os"
	"strconv"
	"strings"
)

// procParentOf reads a process's parent pid and command name from /proc. It
// is the real ParentFunc on Linux; other platforms get the unresolvable
// variant, which keeps the guard's previous parent-pid behaviour.
func procParentOf(pid int) (int, string, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		// The command name is parenthesised and may contain spaces or
		// parentheses, so the fields are taken from the last ')' onwards.
		if close := strings.LastIndexByte(string(raw), ')'); close > 0 {
			fields := strings.Fields(string(raw[close+1:]))
			// After the comm: state, ppid, ...
			if len(fields) >= 2 {
				ppid, perr := strconv.Atoi(fields[1])
				if perr == nil {
					comm := ""
					if open := strings.IndexByte(string(raw), '('); open >= 0 && open < close {
						comm = string(raw[open+1 : close])
					}
					return ppid, comm, true
				}
			}
		}
	}
	return 0, "", false
}
