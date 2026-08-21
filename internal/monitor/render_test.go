package monitor

import (
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/prefs"
	"github.com/stretchr/testify/assert"
)

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

func TestRenderDegradedUsesThePrefsTemplate(t *testing.T) {
	// degraded is a first-class notification kind (issue #66): like every
	// other kind it has a template key, so a consumer can reword or mute it
	// via preferences.json.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
		Event: Event{
			Type:            EventDegraded,
			DegradedSurface: "graphql",
			DegradedMessage: "boom",
		},
		At: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}

	p := prefs.DefaultPreferences()
	n := Render(u, p, time.Minute)
	assert.Equal(t, "⚠️ API degraded (graphql) on o/r#7: boom", n.Message,
		"the default template must keep today's sentence")

	p.Templates["degraded"] = "DEGRADED {degradedSurface} on {prLabel}: {degradedMessage}"
	n = Render(u, p, time.Minute)
	assert.Equal(t, "DEGRADED graphql on o/r#7: boom", n.Message,
		"a user template must be honoured for degraded, like every other kind")
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
