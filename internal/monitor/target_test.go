package monitor

import (
	"encoding/json"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

func TestTargetOfRoundTripsEveryIdentityKind(t *testing.T) {
	tests := []struct {
		name string
		id   resolver.Identity
		kind backend.Kind
	}{
		{
			name: "pr",
			id:   resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", Number: 42, Target: "pr"},
			kind: backend.KindPR,
		},
		{
			name: "issue",
			id:   resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", Number: 7, Target: "issue"},
			kind: backend.KindIssue,
		},
		{
			name: "ref",
			id:   resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", Ref: "main", Target: "ref"},
			kind: backend.KindRef,
		},
		{
			name: "commit",
			id:   resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", CommitSHA: "abc1234", Target: "commit"},
			kind: backend.KindCommit,
		},
		{
			name: "run",
			id:   resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", RunID: 99, Target: "run"},
			kind: backend.KindRun,
		},
		{
			name: "repo",
			id:   resolver.Identity{Owner: "o", Repo: "r", Host: "github.com", Target: "repo"},
			kind: backend.KindRepo,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tgt := TargetOf(tc.id)
			if tgt.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", tgt.Kind, tc.kind)
			}
			if got := IdentityOf(tgt); got != tc.id {
				t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, tc.id)
			}
		})
	}
}

func TestTargetOfDefaultsEmptyTargetToPR(t *testing.T) {
	// resolver.Resolve leaves Target empty when it parses a pull request URL,
	// and every caller treats an empty target as a PR. The conversion has to
	// agree, or a URL-specified PR would resolve to no backend at all.
	tgt := TargetOf(resolver.Identity{Owner: "o", Repo: "r", Number: 1})
	if tgt.Kind != backend.KindPR {
		t.Fatalf("Kind = %q, want pr", tgt.Kind)
	}
}

func TestStatusTypesReportTheirKind(t *testing.T) {
	cases := []struct {
		status backend.Status
		want   backend.Kind
	}{
		{&PRStatus{}, backend.KindPR},
		{&IssueStatus{}, backend.KindIssue},
		{&RunStatus{}, backend.KindRun},
		{&RefStatus{}, backend.KindRef},
		{&RepoStatus{}, backend.KindRepo},
	}
	for _, c := range cases {
		if got := c.status.TargetKind(); got != c.want {
			t.Fatalf("%T.TargetKind() = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestStatusDecodersAreRegisteredForEveryKind(t *testing.T) {
	// An Update that crosses a process boundary carries its Status encoded.
	// Without a decoder the status would silently arrive as nil, so every
	// kind the built-in backend produces must have one.
	for _, k := range backend.AllKinds() {
		st, err := backend.DecodeStatus(k, []byte(`{}`))
		if err != nil {
			t.Fatalf("DecodeStatus(%q): %v", k, err)
		}
		if st == nil {
			t.Fatalf("no status decoder registered for kind %q", k)
		}
		if k != backend.KindCommit && st.TargetKind() != k {
			t.Fatalf("decoded kind = %q, want %q", st.TargetKind(), k)
		}
	}
}

func TestUpdateRoundTripsStatusThroughJSON(t *testing.T) {
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1},
		Event:  backend.Event{Type: backend.EventNewFailingChecks, Checks: []string{"build"}},
		Status: &PRStatus{State: "OPEN", FailingChecks: []string{"build"}},
	}

	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got backend.Update
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	pr, ok := got.Status.(*PRStatus)
	if !ok {
		t.Fatalf("Status decoded as %T, want *PRStatus", got.Status)
	}
	if pr.State != "OPEN" || len(pr.FailingChecks) != 1 || pr.FailingChecks[0] != "build" {
		t.Fatalf("status did not survive the round trip: %+v", pr)
	}
	if got.Event.Type != backend.EventNewFailingChecks {
		t.Fatalf("event type = %q", got.Event.Type)
	}
}

func TestUpdateWithoutStatusRoundTrips(t *testing.T) {
	// A backend that knows what changed without holding a full snapshot
	// leaves Status nil; that has to survive encoding rather than error.
	u := backend.Update{
		Target: backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1},
		Event:  backend.Event{Type: backend.EventMerged},
	}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got backend.Update
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != nil {
		t.Fatalf("Status = %#v, want nil", got.Status)
	}
}
