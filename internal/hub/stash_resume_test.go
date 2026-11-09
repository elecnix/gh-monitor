package hub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/monitor"
)

// TestStashedResumeReturnsTheWatchersBaseline is the continuity half of issue
// #216: a watch that replaced itself with an upgraded binary reconnects with
// the same resume ID and must get its baseline back instead of a first poll.
// Before this, the resume table was filled only by a daemon handoff, so a
// same-daemon reconnect found nothing and replayed state the operator had
// already been shown.
func TestStashedResumeReturnsTheWatchersBaseline(t *testing.T) {
	h := New(nil, nil, time.Minute, nil)

	want := `{"state":"OPEN","title":"already reported"}`
	st := &monitor.IssueStatus{}
	if err := json.Unmarshal([]byte(want), st); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	s := &sub{
		resumeID: "watch-1",
		target: backend.Target{
			Kind: backend.KindIssue, Owner: "patty-io", Repo: "pi-patty-bg-tasks", Number: 32,
		},
		watchOpts: backend.WatchOptions{Interval: time.Minute},
		handle: consumerHandle{
			baseline: func() backend.Status { return st },
		},
	}
	h.stashResume(s)

	raw, ok := h.takeResume("watch-1")
	if !ok {
		t.Fatal("a detached watcher's baseline was not held for its reconnect")
	}
	if len(raw) == 0 {
		t.Fatal("the held baseline is empty")
	}
	var got monitor.IssueStatus
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode held baseline: %v", err)
	}
	if got.State != "OPEN" || got.Title != "already reported" {
		t.Fatalf("held baseline = %+v, want the watcher's last shown state", got)
	}

	// Consumed exactly once: a second reconnect with the same ID gets a fresh
	// first poll rather than a stale replay.
	if _, ok := h.takeResume("watch-1"); ok {
		t.Fatal("the baseline was returned twice")
	}
}

// TestStashIgnoresWatchersWithoutAResumeID keeps one-shot watches out of the
// resume table entirely.
func TestStashIgnoresWatchersWithoutAResumeID(t *testing.T) {
	h := New(nil, nil, time.Minute, nil)
	s := &sub{
		target:    backend.Target{Kind: backend.KindIssue, Owner: "o", Repo: "r", Number: 1},
		watchOpts: backend.WatchOptions{Interval: time.Minute},
		handle:    consumerHandle{},
	}
	h.stashResume(s)
	if len(h.resumes) != 0 {
		t.Fatalf("resume table has %d entries, want 0", len(h.resumes))
	}
}
