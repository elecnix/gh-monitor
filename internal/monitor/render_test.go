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

func TestRenderDegradedNamesBlindSharedSurfaces(t *testing.T) {
	// Issue #98: a failed PR query stops delivering check outcomes, head
	// commit, and mergeability — surfaces the tier system never sheds — so
	// the rendered notice must name them, not just the API surface.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
		Event: Event{
			Type:             EventDegraded,
			DegradedSurface:  "graphql",
			DegradedMessage:  "boom",
			DegradedSurfaces: []string{"check outcomes", "head commit", "mergeability"},
		},
		At: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	n := Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.Contains(t, n.Message, "check outcomes, head commit, mergeability",
		"the built-in sentence must name what the failed query stopped delivering")

	// The structured field is also available as a template token.
	p := prefs.DefaultPreferences()
	p.Templates["degraded"] = "down ({degradedSurface}); blind: {degradedSurfaces}"
	n = Render(u, p, time.Minute)
	assert.Equal(t, "down (graphql); blind: check outcomes, head commit, mergeability", n.Message,
		"{degradedSurfaces} must be interpolable like every other token")

	// A degraded event with no blind-surface list keeps the sentence clean:
	// a backend transport break has no per-surface claims to make.
	u.Event.DegradedSurfaces = nil
	n = Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.NotContains(t, n.Message, "no longer delivering",
		"an empty blind-surface list must not append an empty clause")
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

// TestRenderNewCommitNamesCoauthors pins the rendered new-commit message
// (issue #179): it names the commit's co-authors, and the clause is omitted
// entirely when the commit carries no Co-authored-by trailer — never rendered
// as a dangling "co-authored by ."
func TestRenderNewCommitNamesCoauthors(t *testing.T) {
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	commit := func(coauthors ...string) *backend.CommitSummary {
		return &backend.CommitSummary{
			Oid:             "abc1234def5678",
			ShortOid:        "abc1234",
			Author:          "alice",
			Coauthors:       coauthors,
			MessageHeadline: "feat: x",
		}
	}
	update := func(c *backend.CommitSummary) backend.Update {
		return backend.Update{
			Target: backend.Target{Kind: backend.KindPR, Owner: "owner", Repo: "repo", Number: 42},
			Event:  Event{Type: EventNewCommit, Commit: c},
			At:     at,
		}
	}

	n := Render(update(commit("bob")), prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t,
		"📝 New commit abc1234 pushed to owner/repo#42 by alice, co-authored by bob. "+
			"Review the PR description to ensure it still reflects the latest changes.",
		n.Message)

	n = Render(update(commit("bob", "carol")), prefs.DefaultPreferences(), time.Minute)
	assert.Contains(t, n.Message, "co-authored by bob, carol",
		"every co-author is named, not only the first")

	n = Render(update(commit()), prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t,
		"📝 New commit abc1234 pushed to owner/repo#42 by alice. "+
			"Review the PR description to ensure it still reflects the latest changes.",
		n.Message)
	assert.NotContains(t, n.Message, "co-authored",
		"a commit with no co-authors must not carry an empty clause")
}

func TestFirstPollAckSubstitutesPollModeForEveryTargetKind(t *testing.T) {
	// The default first-poll template is "📡 Monitoring {prLabel} ({pollMode})".
	// Only the PR vars builder populated {pollMode}, so every other watch kind
	// rendered the literal token into its first-poll ack — an issue watch read
	// "📡 Monitoring patty-io/pi-patty-bg-tasks#32 ({pollMode})".
	// No watch kind may leak an unsubstituted token into that ack.
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		update backend.Update
		want   string
	}{
		{
			name: "issue",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindIssue, Owner: "patty-io", Repo: "pi-patty-bg-tasks", Number: 32},
				Event:  Event{Type: EventFirstPoll},
				Status: &IssueStatus{State: "OPEN", Title: "t"},
				At:     at,
			},
			want: "📡 Monitoring patty-io/pi-patty-bg-tasks#32 (polling every 60s)",
		},
		{
			name: "ref",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindRef, Owner: "o", Repo: "r", Ref: "main"},
				Event:  Event{Type: EventFirstPoll},
				Status: &RefStatus{Oid: "abcdef1234", ShortOid: "abcdef1"},
				At:     at,
			},
			want: "📡 Monitoring o/r@main (polling every 60s)",
		},
		{
			name: "repo",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindRepo, Owner: "o", Repo: "r"},
				Event:  Event{Type: EventFirstPoll},
				Status: &RepoStatus{},
				At:     at,
			},
			want: "📡 Monitoring o/r (polling every 60s)",
		},
		{
			name: "pr",
			update: backend.Update{
				Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 7},
				Event:  Event{Type: EventFirstPoll},
				Status: &PRStatus{},
				At:     at,
			},
			want: "📡 Monitoring o/r#7 (polling every 60s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := Render(tc.update, prefs.DefaultPreferences(), time.Minute)
			assert.Equal(t, "first-poll", n.Type)
			assert.NotContains(t, n.Message, "{pollMode}",
				"the ack must not leak an unsubstituted template token")
			assert.Equal(t, tc.want, n.Message)
		})
	}
}

func TestFirstPollAckForAnIssueQuotesTheRoutedPollMode(t *testing.T) {
	// A daemon-served watch reports how it is actually served (route.go's
	// PollMode); the issue ack must quote that mode, not the client interval.
	// This is the value the literal {pollMode} stood in for.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindIssue, Owner: "o", Repo: "r", Number: 5},
		Event: Event{
			Type:     EventFirstPoll,
			PollMode: "webhook via broker-subscriber, safety check every 30m",
		},
		Status: &IssueStatus{State: "OPEN"},
		At:     time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	n := Render(u, prefs.DefaultPreferences(), time.Minute)
	assert.Equal(t, "📡 Monitoring o/r#5 (webhook via broker-subscriber, safety check every 30m)", n.Message)
}
