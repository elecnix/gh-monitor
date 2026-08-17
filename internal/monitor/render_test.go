package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func watchTestOpts() RunOptions {
	return RunOptions{
		Identity: resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", Number: 7, Target: "pr"},
		Prefs:    prefs.DefaultPreferences(),
		Interval: 10 * time.Second,
		Now:      func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
}

func TestWatchEmitsUpdatesCarryingEventAndStatus(t *testing.T) {
	svc := &Service{API: scriptedAPI([]*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", []string{"build"}),
		mkPR("MERGED", true, "aaaaaaa", []string{"build"}),
	})}

	var got []backend.Update
	err := Watch(context.Background(), svc, watchTestOpts(), func(u backend.Update) {
		got = append(got, u)
	})
	require.NoError(t, err)
	require.NotEmpty(t, got)

	for _, u := range got {
		assert.Equal(t, backend.KindPR, u.Target.Kind, "every update names its target")
		assert.Equal(t, "o", u.Target.Owner)
		assert.Equal(t, 7, u.Target.Number)
		assert.NotEmpty(t, u.Event.Type, "every update carries an event kind")
	}

	// The first update is the baseline, and it carries the snapshot it was
	// taken from — that is what lets a consumer render without re-reading.
	assert.Equal(t, backend.EventFirstPoll, got[0].Event.Type)
	st, ok := got[0].Status.(*PRStatus)
	require.True(t, ok, "first-poll update should carry a *PRStatus")
	assert.Equal(t, []string{"build"}, st.FailingChecks)

	// Reaching a terminal state is marked on the update, so a forwarder knows
	// the stream is finished without having to interpret the event kind.
	last := got[len(got)-1]
	assert.Equal(t, backend.EventMerged, last.Event.Type)
	assert.True(t, last.Terminal, "a merged PR must mark the update terminal")
}

func TestWatchAndRunAgreeExactly(t *testing.T) {
	// Run is defined as Watch plus Render. Proving they agree on the same
	// script is what lets the rest of the suite keep asserting on Run.
	script := []*PullRequest{
		mkPR("OPEN", false, "aaaaaaa", []string{"build"}),
		mkPR("OPEN", false, "bbbbbbb", nil),
		mkPR("MERGED", true, "bbbbbbb", nil),
	}

	var viaRun []Notification
	require.NoError(t, Run(context.Background(), &Service{API: scriptedAPI(script)}, watchTestOpts(),
		func(n Notification) { viaRun = append(viaRun, n) }))

	opts := watchTestOpts()
	var viaWatch []Notification
	require.NoError(t, Watch(context.Background(), &Service{API: scriptedAPI(script)}, opts,
		func(u backend.Update) { viaWatch = append(viaWatch, Render(u, opts.Prefs, opts.Interval)) }))

	assert.Equal(t, viaRun, viaWatch)
}

func TestRenderUsesTheEventNoticeVerbatim(t *testing.T) {
	// A notice is a diagnostic about the watcher, not about the target: there
	// is no state to interpolate, so no template applies.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
		Event:  Event{Type: EventDegraded, Notice: "⚠️ something specific happened"},
		At:     time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	n := Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t, "degraded", n.Type)
	assert.Equal(t, "⚠️ something specific happened", n.Message)
	assert.Equal(t, "o/r#7", n.PRLabel)
}

func TestRenderBuildsADegradedMessageFromEventFields(t *testing.T) {
	// A backend that reports it could not read a surface does not have to
	// pre-render the sentence; the renderer builds it from the fields.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
		Event: Event{
			Type:            EventDegraded,
			DegradedSurface: "graphql",
			DegradedMessage: "boom",
		},
		At: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	n := Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t, "⚠️ API degraded (graphql) on o/r#7: boom", n.Message)
}

func TestRenderWithoutStatusStillProducesAMessage(t *testing.T) {
	// A backend that knows what changed without holding a snapshot leaves
	// Status nil. That must render from the event alone rather than panic.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
		Event:  Event{Type: EventNewFailingChecks, Checks: []string{"build", "lint"}},
		At:     time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	n := Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t, "new-failing-checks", n.Type)
	assert.Equal(t, "o/r#7", n.PRLabel)
	assert.NotEmpty(t, n.Message)
	assert.Equal(t, []string{"build", "lint"}, n.FailingChecks)
}

func TestRenderPrefersAnEventSuppliedDetail(t *testing.T) {
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindRun, Owner: "o", Repo: "r", RunID: 3},
		Event: Event{
			Type:          EventRunCompleted,
			RunConclusion: "failure",
			Detail:        "job\tsomething exploded",
		},
		Status: &RunStatus{RunID: 3, RunNumber: 3, Status: "completed", Conclusion: "failure"},
		At:     time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	n := Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t, "job\tsomething exploded", n.Detail)
	assert.Equal(t, "failure", n.Conclusion)
}

func TestRenderDispatchesOnTargetKind(t *testing.T) {
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		update  backend.Update
		wantLbl string
	}{
		{
			name: "issue",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindIssue, Owner: "o", Repo: "r", Number: 5},
				Event:  Event{Type: EventIssueNewComment},
				Status: &IssueStatus{State: "OPEN", Title: "t"},
				At:     at,
			},
			wantLbl: "o/r#5",
		},
		{
			name: "ref",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindRef, Owner: "o", Repo: "r", Ref: "main"},
				Event:  Event{Type: EventNewFailingChecks, Checks: []string{"build"}},
				Status: &RefStatus{Oid: "abcdef1234", ShortOid: "abcdef1"},
				At:     at,
			},
			wantLbl: "o/r@main",
		},
		{
			name: "repo",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindRepo, Owner: "o", Repo: "r"},
				Event:  Event{Type: EventRepoNewPR, RepoItems: []RepoItemSummary{{Number: 9, Title: "x", Author: "a", URL: "u"}}},
				Status: &RepoStatus{},
				At:     at,
			},
			wantLbl: "o/r#9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := Render(tc.update, prefs.DefaultPreferences(), time.Minute)
			assert.Equal(t, tc.wantLbl, n.PRLabel)
			assert.NotEmpty(t, n.Message)
		})
	}
}
