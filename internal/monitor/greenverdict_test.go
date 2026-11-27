package monitor

import (
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The green verdict must rest on checks the watch saw pass, on a head commit
// it had already read (issue #239). A check that a rerun or a push removed
// from the rollup is neither failing nor pending, and before this fix that
// absence read as a pass.

// mkRun builds one check run in a state GitHub actually reports.
func mkRun(name, status, conclusion string) CheckRun {
	return CheckRun{Name: name, Status: status, Conclusion: conclusion}
}

func passed(name string) CheckRun  { return mkRun(name, "COMPLETED", "SUCCESS") }
func failed(name string) CheckRun  { return mkRun(name, "COMPLETED", "FAILURE") }
func running(name string) CheckRun { return mkRun(name, "IN_PROGRESS", "") }

// mkChecksPR builds an open PR whose head commit oid carries one completed
// Actions suite holding runs. With no runs it has no suite at all: that is
// what GitHub reports for a commit whose check suites do not exist yet.
func mkChecksPR(oid string, runs ...CheckRun) *PullRequest {
	commit := mkCommit(oid, nil)
	if len(runs) == 0 {
		commit.Commit.CheckSuites = SuiteNodes{}
	} else {
		suite := &commit.Commit.CheckSuites.Nodes[0]
		suite.CheckRuns = RunNodes{Nodes: runs}
		suite.Conclusion = ""
		for _, r := range runs {
			if r.Status != "COMPLETED" {
				suite.Status = "IN_PROGRESS"
			}
		}
	}
	return &PullRequest{State: "OPEN", Commits: CommitNodes{Nodes: []Commit{commit}}}
}

var greenReadAt = time.Date(2026, 11, 27, 9, 45, 37, 0, time.UTC)

func greenOpts() RunOptions {
	return RunOptions{
		Identity: resolver.Identity{Owner: "o", Repo: "r", Number: 190},
		Prefs:    prefs.DefaultPreferences(),
		Now:      func() time.Time { return greenReadAt },
	}
}

// consumePolls feeds each payload to one consumer, in order, and returns the
// updates of each poll after the first.
func consumePolls(t *testing.T, polls ...*PullRequest) [][]backend.Update {
	t.Helper()
	c := NewPRConsumer(greenOpts())
	var out [][]backend.Update
	for i, pr := range polls {
		var got []backend.Update
		c.Consume(Snapshot(pr, SnapshotOptions{}), func(u backend.Update) { got = append(got, u) })
		if i > 0 {
			out = append(out, got)
		}
	}
	return out
}

func greenIn(updates []backend.Update) *backend.Update {
	for i := range updates {
		if updates[i].Event.Type == EventCIAllGreen {
			return &updates[i]
		}
	}
	return nil
}

func TestGreenVerdict_VanishedCheckIsNotAPass(t *testing.T) {
	polls := consumePolls(t,
		mkChecksPR("2b0aa6e1", passed("build"), failed("cite")),
		// `gh run rerun --failed` removed cite's run; the new one is not created yet.
		mkChecksPR("2b0aa6e1", passed("build")),
		mkChecksPR("2b0aa6e1", passed("build"), running("cite")),
		mkChecksPR("2b0aa6e1", passed("build"), passed("cite")),
	)
	assert.Nil(t, greenIn(polls[0]), "a check that vanished must not read as passed")
	assert.Nil(t, greenIn(polls[1]), "cite is back and still running")
	g := greenIn(polls[2])
	require.NotNil(t, g, "green once cite reports a pass")
	assert.Equal(t, []string{"build", "cite"}, g.Event.PassedChecks)
}

func TestGreenVerdict_VanishedCheckThatReturnsPassedStillGoesGreen(t *testing.T) {
	// The rerun finished between two polls: the watch never saw cite pending.
	polls := consumePolls(t,
		mkChecksPR("2b0aa6e1", passed("build"), failed("cite")),
		mkChecksPR("2b0aa6e1", passed("build")),
		mkChecksPR("2b0aa6e1", passed("build"), passed("cite")),
	)
	assert.Nil(t, greenIn(polls[0]))
	require.NotNil(t, greenIn(polls[1]), "the green withheld for the missing check arrives once it passes")
}

func TestGreenVerdict_EmptyRollupIsNotAPass(t *testing.T) {
	polls := consumePolls(t,
		mkChecksPR("aaaa1111", running("build")),
		mkChecksPR("aaaa1111"),
		mkChecksPR("aaaa1111", passed("build")),
	)
	assert.Nil(t, greenIn(polls[0]), "a rollup with no checks at all is not green")
	require.NotNil(t, greenIn(polls[1]))
}

func TestGreenVerdict_NotInTheSamePollAsAPush(t *testing.T) {
	// The verdict belongs to the commit it was computed on. A poll that is the
	// first to read a new head reports new-commit, not a green for that head.
	polls := consumePolls(t,
		mkChecksPR("aaaa1111", passed("build"), failed("cite")),
		mkChecksPR("bbbb2222", passed("build"), passed("cite")),
		mkChecksPR("bbbb2222", passed("build"), passed("cite")),
	)
	types := func(us []backend.Update) []backend.EventType {
		var ts []backend.EventType
		for _, u := range us {
			ts = append(ts, u.Event.Type)
		}
		return ts
	}
	assert.Contains(t, types(polls[0]), EventNewCommit)
	assert.Nil(t, greenIn(polls[0]), "no green in the same breath as a push")
	g := greenIn(polls[1])
	require.NotNil(t, g, "green on the next read of the same head")
	require.NotNil(t, g.Event.Commit)
	assert.Equal(t, "bbbb2222", g.Event.Commit.Oid)
}

func TestGreenVerdict_OrdinaryGreenNamesChecksCommitAndReadTime(t *testing.T) {
	polls := consumePolls(t,
		mkChecksPR("2b0aa6e1c0ffee", passed("build"), running("lint")),
		mkChecksPR("2b0aa6e1c0ffee", passed("build"), passed("lint")),
	)
	g := greenIn(polls[0])
	require.NotNil(t, g)
	assert.Equal(t, []string{"build", "lint"}, g.Event.PassedChecks)

	n := Render(*g, prefs.DefaultPreferences(), 0)
	assert.Equal(t, "✅ CI green on o/r#190 at 2b0aa6e: 2 checks passed (build, lint), none failing or pending, read 2026-11-27T09:45:37Z", n.Message)
	assert.Equal(t, []string{"build", "lint"}, n.PassedChecks)
	assert.Equal(t, "2b0aa6e1c0ffee", n.HeadSHA)
	assert.Equal(t, greenReadAt, n.Timestamp)
}

func TestGreenVerdict_FirstPollGreenNamesItsEvidence(t *testing.T) {
	c := NewPRConsumer(greenOpts())
	var got []backend.Update
	c.Consume(Snapshot(mkChecksPR("2b0aa6e1", passed("build")), SnapshotOptions{}), func(u backend.Update) { got = append(got, u) })
	g := greenIn(got)
	require.NotNil(t, g)
	assert.Equal(t, []string{"build"}, g.Event.PassedChecks)
	require.NotNil(t, g.Event.Commit)
	assert.Equal(t, "2b0aa6e1", g.Event.Commit.Oid)
}

func TestGreenVerdict_RefWatch(t *testing.T) {
	ref := func(oid string, failing, pending, successful []string) *RefStatus {
		return &RefStatus{Oid: oid, ShortOid: oid, FailingChecks: failing, PendingChecks: pending, SuccessfulChecks: successful}
	}
	t.Run("vanished check", func(t *testing.T) {
		prev := ref("a", []string{"cite"}, nil, []string{"build"})
		mid := ref("a", nil, nil, []string{"build"})
		assert.Nil(t, findEvent(DiffRef(prev, mid), EventCIAllGreen))
		last := ref("a", nil, nil, []string{"build", "cite"})
		e := findEvent(DiffRef(mid, last), EventCIAllGreen)
		require.NotNil(t, e)
		assert.Equal(t, []string{"build", "cite"}, e.PassedChecks)
	})
	t.Run("empty rollup after a push", func(t *testing.T) {
		prev := ref("a", nil, []string{"build"}, nil)
		curr := ref("b", nil, nil, nil)
		assert.Nil(t, findEvent(DiffRef(prev, curr), EventCIAllGreen))
	})
}

func TestGreenVerdict_RenderAnEventWithoutEvidence(t *testing.T) {
	// A sub-daemon built before issue #239 sends a bare ci-all-green.
	target := TargetOf(greenOpts().Identity)
	t.Run("filled from the status it carries", func(t *testing.T) {
		u := backend.Update{Target: target, At: greenReadAt, Event: Event{Type: EventCIAllGreen},
			Status: &PRStatus{SuccessfulChecks: []string{"build"}, LastCommit: CommitSummary{Oid: "2b0aa6e1c0ffee", ShortOid: "2b0aa6e"}}}
		n := Render(u, prefs.DefaultPreferences(), 0)
		assert.Equal(t, []string{"build"}, n.PassedChecks)
		assert.Equal(t, "2b0aa6e1c0ffee", n.HeadSHA)
		assert.Contains(t, n.Message, "1 checks passed (build)")
		assert.False(t, n.GreenUnverified)
	})
	t.Run("says so when nothing names a passed check", func(t *testing.T) {
		u := backend.Update{Target: target, At: greenReadAt, Event: Event{Type: EventCIAllGreen}}
		n := Render(u, prefs.DefaultPreferences(), 0)
		assert.Empty(t, n.PassedChecks)
		assert.True(t, n.GreenUnverified)
		assert.Contains(t, n.Message, "unverified")
	})
}
