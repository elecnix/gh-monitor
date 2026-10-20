package mux

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/testrec"
)

// failoverNote is one recorded failover.
type failoverNote struct {
	target backend.Target
	from   string
	reason string
	at     time.Time
}

// failoverRecorder collects the router's failover observations. It is safe
// for concurrent use: several watches fail over at once.
type failoverRecorder struct {
	notes testrec.Recorder[failoverNote]
}

// record notes one failover.
func (r *failoverRecorder) record(target backend.Target, from, reason string, at time.Time) {
	r.notes.Add(failoverNote{target: target, from: from, reason: reason, at: at})
}

// all returns the recorded failovers, oldest first.
func (r *failoverRecorder) all() []failoverNote {
	return r.notes.All()
}

// TestRoutingSourceStampsSubdaemonUpdates proves that every update a
// sub-daemon delivers is marked with the sub-daemon's name (issue #122). A
// log that cannot tell a sub-daemon-served update from a hub-served one
// cannot say which path produced it — and the two cost very differently.
func TestRoutingSourceStampsSubdaemonUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := &recordingSource{
		hold:    true,
		updates: []backend.Update{{At: time.Now(), Event: backend.Event{Type: backend.EventReviewApproved}}},
	}
	reg := probedRegistry(t, ctx, shortSock(t, "stamped.sock"), []backend.Kind{backend.KindPR}, sub)

	fallback := &recordingSource{hold: true}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 9},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}

	// The sub-daemon's own update must carry its name as the source. The
	// backlog the hub served comes first, so read until the stamped update
	// arrives.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("the stream closed before the sub-daemon's update arrived")
			}
			if u.Event.Type == backend.EventReviewApproved {
				if u.Source != "fakebroker" {
					t.Fatalf("a sub-daemon-served update must name the sub-daemon, got %q", u.Source)
				}
				return
			}
		case <-deadline:
			t.Fatal("the sub-daemon's update never arrived")
		}
	}
}

// TestRoutingSourceObservesFailover proves that a watch falling back from a
// sub-daemon to the hub is reported (issue #122). The fallback is not free:
// the sub-daemon served the watch through a webhook at no API cost, and the
// hub takes over with a poll, so the fallbacks are what the log has to show.
func TestRoutingSourceObservesFailover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// recordingSource closes at once: the stream ends with no terminal
	// update, which is the failover case.
	reg := probedRegistry(t, ctx, shortSock(t, "fails.sock"), []backend.Kind{backend.KindPR}, &recordingSource{})

	rec := &failoverRecorder{}
	fallback := &recordingSource{hold: true}
	rs := RoutingSource{Reg: reg, Fallback: fallback, OnFailover: rec.record}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 5},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain until the failover notice arrives.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("the stream closed before the failover notice arrived")
			}
			if u.Event.Type == backend.EventDegraded && strings.Contains(u.Event.Notice, "fakebroker") {
				goto sawNotice
			}
		case <-deadline:
			t.Fatal("no failover notice")
		}
	}
sawNotice:
	notes := rec.all()
	if len(notes) != 1 {
		t.Fatalf("the failover must be observed exactly once, got %d", len(notes))
	}
	n := notes[0]
	if n.from != "fakebroker" {
		t.Errorf("the observation names the sub-daemon that stopped serving, got %q", n.from)
	}
	if n.target.Owner != "o" || n.target.Repo != "r" || n.target.Number != 5 {
		t.Errorf("the observation names the watch that moved, got %+v", n.target)
	}
	if n.reason == "" {
		t.Error("the observation says why the watch moved")
	}
	if n.at.IsZero() {
		t.Error("the observation says when the watch moved")
	}
}

// TestRoutingSourceObservesDialFailure proves that a sub-daemon that cannot
// be dialed at all is also reported as a failover: the watch ends up on the
// hub either way, and the hub's poll is spent either way.
func TestRoutingSourceObservesDialFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := shortSock(t, "gone.sock")
	reg := probedRegistry(t, ctx, sock, []backend.Kind{backend.KindPR}, &recordingSource{})
	// The sub-daemon dies between the probe and the watch.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}

	rec := &failoverRecorder{}
	fallback := &recordingSource{hold: true}
	rs := RoutingSource{Reg: reg, Fallback: fallback, OnFailover: rec.record}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 4},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatalf("a failed dial must fall back, not error: %v", err)
	}
	// Drain the backlog and the notice.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatal("the stream closed before the fallback notice arrived")
			}
		case <-deadline:
			t.Fatal("no fallback notice")
		}
		// The failover is observed as soon as the dial fails, which happens
		// before the backlog is read; give it a moment and check.
		if len(rec.all()) > 0 {
			break
		}
	}
	notes := rec.all()
	if len(notes) != 1 {
		t.Fatalf("the dial failure must be observed exactly once, got %d", len(notes))
	}
	if notes[0].from != "fakebroker" {
		t.Errorf("the observation names the sub-daemon that could not be dialed, got %q", notes[0].from)
	}
}

// TestRoutingSourceWithoutObserverIsUnaffected proves the observer is
// optional: a router with no observer behaves exactly as before, and never
// panics on a failover.
func TestRoutingSourceWithoutObserverIsUnaffected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := probedRegistry(t, ctx, shortSock(t, "noobs.sock"), []backend.Kind{backend.KindPR}, &recordingSource{})

	fallback := &recordingSource{hold: true}
	rs := RoutingSource{Reg: reg, Fallback: fallback}
	ch, err := rs.Watch(ctx, backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 3},
		backend.WatchOptions{ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("the stream closed before the failover notice arrived")
			}
			if u.Event.Type == backend.EventDegraded {
				return
			}
		case <-deadline:
			t.Fatal("no failover notice")
		}
	}
}
