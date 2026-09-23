package cmd

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/hub"
	"github.com/elecnix/gh-monitor/internal/ipc"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/mux"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdingSubdaemonSource counts the watches it serves and records their
// ResumeIDs. It emits a first-poll update, then holds a continuous watch open
// until the client leaves; a --once watch ends after the update.
type holdingSubdaemonSource struct {
	watches   atomic.Int64
	resumeIDs chan string
}

func (s *holdingSubdaemonSource) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.watches.Add(1)
	select {
	case s.resumeIDs <- opts.ResumeID:
	default:
	}
	ch := make(chan backend.Update, 1)
	ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()}
	if opts.Once {
		close(ch)
		return ch, nil
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// backlogPR is an open PR with one of each backlog item a first poll must
// report: an unresolved review thread, a general comment and a failing check.
func backlogPR() *monitor.PullRequest {
	line := 3
	pr := &monitor.PullRequest{
		State:     "OPEN",
		Mergeable: "MERGEABLE",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid: "aaaaaaa",
			CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{{
				Status: "COMPLETED", Conclusion: "FAILURE", App: monitor.AppInfo{Name: "ci"},
				CheckRuns: monitor.RunNodes{Nodes: []monitor.CheckRun{{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"}}},
			}}},
		}}}},
	}
	thread := monitor.ReviewThread{ID: "t1", Path: "main.go", Line: &line}
	thread.Comments.Nodes = []monitor.Comment{{ID: "tc1", Body: "please rename this"}}
	thread.Comments.Nodes[0].Author.Login = "reviewer"
	pr.ReviewThreads.Nodes = []monitor.ReviewThread{thread}
	pr.Comments.Nodes = []monitor.Comment{{ID: "c1", Body: "posted before the watch"}}
	pr.Comments.Nodes[0].Author.Login = "reviewer"
	return pr
}

// statusOnlySubdaemon answers a watch the way a webhook-fed sub-daemon does:
// one first-poll update with no per-item events, because it has no record of
// what was posted before it started. A continuous watch then gets one later
// change and stays open until the client leaves.
type statusOnlySubdaemon struct {
	watches atomic.Int64
	opts    chan backend.WatchOptions
}

func (s *statusOnlySubdaemon) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	s.watches.Add(1)
	select {
	case s.opts <- opts:
	default:
	}
	ch := make(chan backend.Update, 2)
	ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventFirstPoll}, At: time.Now()}
	if opts.Once {
		close(ch)
		return ch, nil
	}
	ch <- backend.Update{Target: t, Event: backend.Event{Type: backend.EventReviewApproved}, At: time.Now()}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestMonitor_SubdaemonWatchStartsWithBacklog is the acceptance test for
// issues #114 and #119. A configured sub-daemon serves a continuous CLI
// watch, ResumeID and all (#114). A sub-daemon that answers with a status-only first poll left
// the client with no backlog: no thread, no comment, no failing check (#119).
// The daemon now fetches the backlog through the hub once, then hands a
// continuous watch to the sub-daemon for the changes after it.
func TestMonitor_SubdaemonWatchStartsWithBacklog(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"once", []string{"7", "-R", "o/r", "--once"}},
		{"continuous", []string{"7", "-R", "o/r", "--timeout", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("GH_MONITOR_AUTOSTART", "0")
			serveCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			sub := &statusOnlySubdaemon{opts: make(chan backend.WatchOptions, 1)}
			brokerSock := shortSocket(t, "ghmon-backlog-broker-*.d")
			startFakeSubdaemon(t, serveCtx, brokerSock, []backend.Kind{backend.KindPR}, sub)

			var hubFetches atomic.Int64
			h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
				hubFetches.Add(1)
				return backlogPR(), nil
			}, nil, time.Hour, nil)
			t.Cleanup(h.Stop)

			reg := mux.NewRegistry(io.Discard)
			tr, err := remote.ParseEndpoint("unix:" + brokerSock)
			require.NoError(t, err)
			reg.Track("broker", tr)
			reg.Probe(serveCtx)
			require.NotNil(t, reg.Provider(backend.KindPR))

			sock := shortSocket(t, "ghmon-backlog-*.d")
			l, err := ipc.Listen(sock)
			require.NoError(t, err)
			bindTestServerOnWithRoutes(t, serveCtx, h, l, reg)
			t.Setenv("GH_MONITOR_SOCK", sock)

			originalFactory := apiClientFactory
			t.Cleanup(func() { apiClientFactory = originalFactory })
			apiClientFactory = func(string) ghcli.API { return &commandFakeAPI{} }

			root := newRootCommand()
			stdout := &bytes.Buffer{}
			root.SetOut(stdout)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tc.args)
			require.NoError(t, root.Execute())

			types := notificationTypes(t, stdout.String())
			assert.Contains(t, types, "new-unresolved-threads")
			assert.Contains(t, types, "new-general-comments")
			assert.Contains(t, types, "new-failing-checks")
			first := 0
			for _, typ := range types {
				if typ == "first-poll" {
					first++
				}
			}
			assert.Equal(t, 1, first, "one watch reports one first poll; got %v", types)
			assert.Equal(t, int64(1), hubFetches.Load(), "the backlog costs one hub fetch, and nothing after it")

			if tc.name == "once" {
				assert.Equal(t, int64(0), sub.watches.Load(), "a --once read is the backlog, which only the hub can fetch")
				return
			}
			assert.Equal(t, int64(1), sub.watches.Load(), "the sub-daemon serves the rest of the watch")
			assert.Contains(t, types, "review-approved", "a change after the backlog comes from the sub-daemon")
			opts := <-sub.opts
			assert.NotEmpty(t, opts.ResumeID, "a continuous watch keeps its ResumeID on the routed path")
			assert.Contains(t, opts.Baseline, "posted before the watch", "the sub-daemon is seeded with the hub's snapshot")
		})
	}
}
