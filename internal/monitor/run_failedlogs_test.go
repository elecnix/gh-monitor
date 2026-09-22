package monitor

import (
	"strconv"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizeFailedLog(t *testing.T) {
	t.Run("empty stays empty", func(t *testing.T) {
		assert.Equal(t, "", SummarizeFailedLog("", 50, ""))
		assert.Equal(t, "", SummarizeFailedLog("   \n  ", 50, ""))
	})

	t.Run("under limit cleaned and joined", func(t *testing.T) {
		in := "admin-ci\tbuild\t2026-01-01T00:00:00.000Z step one\nadmin-ci\tbuild\t2026-01-01T00:00:01.000Z step two\n"
		out := SummarizeFailedLog(in, 50, "")
		assert.Equal(t, "build\tstep one", strings.Split(out, "\n")[0])
		assert.Equal(t, "build\tstep two", strings.Split(out, "\n")[1])
		assert.NotContains(t, out, "2026-01-01")
	})

	t.Run("over limit keeps tail with leading marker", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 60; i++ {
			b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z line " + strconv.Itoa(i) + "\n")
		}
		out := SummarizeFailedLog(b.String(), 50, "")
		lines := strings.Split(out, "\n")
		require.Len(t, lines, 51) // marker + 50
		assert.Contains(t, lines[0], "earlier lines truncated")
		assert.Contains(t, lines[0], "10 ")
		// The last kept line is the actual last line of the input.
		assert.Contains(t, lines[50], "line 59")
		// The dropped head is absent.
		assert.NotContains(t, out, "line 0")
	})

	t.Run("over limit names the fetch command when hinted", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 60; i++ {
			b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z line " + strconv.Itoa(i) + "\n")
		}
		hint := "gh run view 30433642 --repo elecnix/gh-monitor --log-failed"
		out := SummarizeFailedLog(b.String(), 50, hint)
		lines := strings.Split(out, "\n")
		require.Len(t, lines, 51) // marker + 50: the hint must not add lines
		assert.Equal(t, "… (10 earlier lines truncated — full failed-step log: "+hint+")", lines[0])
		// One marker, not one per dropped line.
		assert.Equal(t, 1, strings.Count(out, "truncated"))
		assert.Contains(t, lines[50], "line 59")
	})

	t.Run("exactly at limit no marker", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 50; i++ {
			b.WriteString("wf\tjob\t2026-01-01T00:00:00.000Z line " + strconv.Itoa(i) + "\n")
		}
		out := SummarizeFailedLog(b.String(), 50, "gh run view 1 --repo o/r --log-failed")
		lines := strings.Split(out, "\n")
		assert.Len(t, lines, 50)
		assert.NotContains(t, out, "truncated")
		assert.NotContains(t, out, "gh run view")
	})
}

func TestFailedLogHint(t *testing.T) {
	t.Run("github.com leaves the repository bare", func(t *testing.T) {
		id := resolver.Identity{Owner: "elecnix", Repo: "gh-monitor", Host: "github.com"}
		assert.Equal(t, "gh run view 30433642 --repo elecnix/gh-monitor --log-failed", FailedLogHint(id, 30433642))
	})

	t.Run("empty host resolves like github.com", func(t *testing.T) {
		id := resolver.Identity{Owner: "elecnix", Repo: "gh-monitor"}
		assert.Equal(t, "gh run view 7 --repo elecnix/gh-monitor --log-failed", FailedLogHint(id, 7))
	})

	t.Run("enterprise host is kept so the command targets the right server", func(t *testing.T) {
		id := resolver.Identity{Owner: "octo", Repo: "demo", Host: "ghe.example.com"}
		assert.Equal(t, "gh run view 13 --repo ghe.example.com/octo/demo --log-failed", FailedLogHint(id, 13))
	})

	t.Run("incomplete identity or run yields no hint", func(t *testing.T) {
		assert.Equal(t, "", FailedLogHint(resolver.Identity{}, 5))
		assert.Equal(t, "", FailedLogHint(resolver.Identity{Owner: "octo"}, 5))
		assert.Equal(t, "", FailedLogHint(resolver.Identity{Owner: "octo", Repo: "demo"}, 0))
	})
}

func TestCleanFailedLogLine(t *testing.T) {
	t.Run("strips ansi and timestamp, keeps job", func(t *testing.T) {
		// Real ESC byte form.
		in := "admin-ci\tBundle size\t\xef\xbb\xbf2026-07-10T20:54:41.1431776Z \x1b[31mPackage size limit has exceeded by 9.83 kB\x1b[39m"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "Bundle size\tPackage size limit has exceeded by 9.83 kB", out)
		assert.NotContains(t, out, "\x1b[")
		assert.NotContains(t, out, "2026-07-10")
	})

	t.Run("strips caret-notation ansi (gh non-tty form)", func(t *testing.T) {
		// gh run view --log-failed emits `^[[...m` caret notation (no 0x1b) when
		// stdout is not a TTY (e.g. exec.Command). The cleaner must strip it too.
		in := "admin-ci\tBundle size\t2026-07-10T20:54:41.1431776Z ^[[36;1mpnpm size^[[0m done"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "Bundle size\tpnpm size done", out)
		assert.NotContains(t, out, "^[[")
	})

	t.Run("drops the redundant workflow field", func(t *testing.T) {
		in := "admin-ci\tbuild\t2026-01-01T00:00:00.000Z body"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "build\tbody", out)
		assert.NotContains(t, out, "admin-ci")
	})

	t.Run("non-matching line passes through ansi-stripped", func(t *testing.T) {
		in := "\x1b[31mraw line with no tabs\x1b[0m"
		assert.Equal(t, "raw line with no tabs", cleanFailedLogLine(in))
	})

	t.Run("content with embedded tabs is preserved", func(t *testing.T) {
		in := "wf\tjob\t2026-01-01T00:00:00.000Z before\tafter"
		out := cleanFailedLogLine(in)
		assert.Equal(t, "job\tbefore\tafter", out)
	})
}

func TestIsRunFailureConclusion(t *testing.T) {
	for _, c := range []string{"failure", "timed_out", "cancelled", "action_required"} {
		assert.True(t, isRunFailureConclusion(c), c)
	}
	for _, c := range []string{"success", "neutral", "skipped", "stale", ""} {
		assert.False(t, isRunFailureConclusion(c), c)
	}
}
