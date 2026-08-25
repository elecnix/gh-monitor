package subdaemon

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestChildEnvNilHookInheritsBase pins the default: a launcher without a
// ChildEnv hook starts children with the base environment untouched.
func TestChildEnvNilHookInheritsBase(t *testing.T) {
	base := []string{"A=1", "B=2"}
	got := applyChildEnv(base, nil, Entry{Name: "x", Cmd: []string{"/bin/true"}})
	if len(got) != len(base) {
		t.Fatalf("applyChildEnv with a nil hook changed the env: got %v", got)
	}
	for i := range base {
		if got[i] != base[i] {
			t.Fatalf("env[%d] = %q, want %q", i, got[i], base[i])
		}
	}
}

// TestChildEnvHookOverrides pins the hook contract: whatever the hook returns
// becomes the child's environment, whole. The daemon uses this to point each
// sub-daemon at its own private socket (issue #88) without the sub-daemon
// binary knowing anything about it.
func TestChildEnvHookOverrides(t *testing.T) {
	base := []string{"A=1"}
	got := applyChildEnv(base, func(Entry) []string {
		return []string{"GH_MONITOR_SOCK=/tmp/private.sock"}
	}, Entry{Name: "x", Cmd: []string{"/bin/true"}})
	if len(got) != 1 || got[0] != "GH_MONITOR_SOCK=/tmp/private.sock" {
		t.Fatalf("applyChildEnv ignored the hook: got %v", got)
	}
}

// TestLauncherChildEnvReachesTheChild runs the real exec path: a launcher
// with a ChildEnv hook starts `sh -c 'echo $VAR'` and the marker value must
// show up in the child's output, proving the environment was applied.
func TestLauncherChildEnvReachesTheChild(t *testing.T) {
	var out bytes.Buffer
	l := &Launcher{
		Entries: []Entry{{Name: "envy", Cmd: []string{"sh", "-c", "echo MARK=$GH_MONITOR_TEST_MARKER"}}},
		Out:     &out,
		ChildEnv: func(Entry) []string {
			return append(os.Environ(), "GH_MONITOR_TEST_MARKER=private-socket")
		},
		// The child exits immediately (a clean exit under StableRun counts
		// as a rapid failure), so the supervisor settles into its slow-retry
		// loop; cancel the context once the first attempt has run to end it.
		MinBackoff:    time.Millisecond,
		MaxBackoff:    time.Millisecond,
		MaxRapidFails: 1,
		Sleep:         func(time.Duration) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_ = l.Run(ctx)

	if !strings.Contains(out.String(), "MARK=private-socket") {
		t.Fatalf("child did not see the hook's environment; output:\n%s", out.String())
	}
}
