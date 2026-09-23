package cmd

import (
	"bytes"
	"context"
	"errors"
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

// TestMonitor_SubdaemonServesCLIWatches is the end-to-end acceptance test for
// issue #114. Before it, a configured sub-daemon served no watch the CLI
// could send: a continuous watch carries a ResumeID and was sent to the hub,
// and a --once read never dialled the daemon. Both now reach the sub-daemon,
// and neither the hub nor the built-in backend polls GitHub.
func TestMonitor_SubdaemonServesCLIWatches(t *testing.T) {
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

			sub := &holdingSubdaemonSource{resumeIDs: make(chan string, 1)}
			brokerSock := shortSocket(t, "ghmon-cli-broker-*.d")
			startFakeSubdaemon(t, serveCtx, brokerSock, []backend.Kind{backend.KindPR}, sub)

			var hubFetches atomic.Int64
			h := hub.New(func(context.Context, resolver.Identity, monitor.QueryTier) (any, error) {
				hubFetches.Add(1)
				return nil, errors.New("hub must not poll while the sub-daemon serves pr")
			}, nil, time.Hour, nil)
			t.Cleanup(h.Stop)

			reg := mux.NewRegistry(io.Discard)
			tr, err := remote.ParseEndpoint("unix:" + brokerSock)
			require.NoError(t, err)
			reg.Track("broker", tr)
			reg.Probe(serveCtx)
			require.NotNil(t, reg.Provider(backend.KindPR))

			sock := shortSocket(t, "ghmon-cli-*.d")
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

			assert.Equal(t, int64(1), sub.watches.Load(), "the sub-daemon must serve the watch")
			assert.Equal(t, int64(0), hubFetches.Load(), "the hub must not poll a kind the sub-daemon serves")
			assert.Contains(t, stdout.String(), "first-poll")
			if tc.name == "continuous" {
				assert.NotEmpty(t, <-sub.resumeIDs, "a continuous watch keeps its ResumeID on the routed path")
			}
		})
	}
}
