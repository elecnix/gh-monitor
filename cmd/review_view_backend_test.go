package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/backend/remote"
	"github.com/elecnix/gh-monitor/internal/ghcli"
)

// remoteReport is an external backend's review-summary capability. It records
// what it was asked for so a test can assert the command resolved the target
// correctly on its way out.
type remoteReport struct {
	mu      sync.Mutex
	targets []backend.Target
	opts    []backend.ReportOptions
}

func (r *remoteReport) ViewReport(_ context.Context, t backend.Target, opts backend.ReportOptions) (*backend.Report, error) {
	r.mu.Lock()
	r.targets = append(r.targets, t)
	r.opts = append(r.opts, opts)
	r.mu.Unlock()

	body := "looks good to me"
	return &backend.Report{Reviews: []backend.ReportReview{{
		ID:          "PRR_from_backend",
		State:       backend.ReportStateApproved,
		AuthorLogin: opts.Reviewer,
		Body:        &body,
	}}}, nil
}

func (r *remoteReport) seen() ([]backend.Target, []backend.ReportOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]backend.Target(nil), r.targets...), append([]backend.ReportOptions(nil), r.opts...)
}

// startReportBackend serves remote.Serve with a report-only capability on a
// unix socket. The built-in backend stays registered underneath it, so a test
// that passes here proves the external one actually won resolution rather
// than the command quietly falling back to the gh client.
func startReportBackend(t *testing.T, ctx context.Context, sockPath string) *remoteReport {
	t.Helper()
	actor := &remoteReport{}

	l, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = remote.Serve(ctx, c, remote.ServerConfig{
					Name: "reportbox", Kinds: []backend.Kind{backend.KindPR}, Report: actor,
				})
			}(conn)
		}
	}()
	return actor
}

// `review view` is a capability like every other mutation verb, so an external
// backend that registers one serves it. Before this it built a report service
// from apiClientFactory directly: no registry, no backend flags, and
// `gh monitor review view --backend-endpoint ...` failed with "unknown flag".
func TestReviewViewIsServedByAnExternalBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sock := shortSocket(t, "ghmon-report-*.d")
	actor := startReportBackend(t, ctx, sock)

	// The gh client must never be consulted for the report itself. It is
	// still registered, so a command that ignored the external backend would
	// fail here rather than quietly succeed.
	originalFactory := apiClientFactory
	t.Cleanup(func() { apiClientFactory = originalFactory })
	apiClientFactory = func(string) ghcli.API {
		t.Fatal("review view went to the built-in backend despite --backend-endpoint")
		return nil
	}

	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{
		"review", "view",
		"--backend-endpoint", "unix:" + sock,
		"--repo", "agyn/repo", "--reviewer", "alice", "--unresolved",
		"51",
	})
	require.NoError(t, root.Execute())

	targets, opts := actor.seen()
	require.Len(t, targets, 1, "the external backend served exactly one report")
	assert.Equal(t, backend.Target{Kind: backend.KindPR, Owner: "agyn", Repo: "repo", Number: 51, Host: "github.com"}, targets[0],
		"the command resolved the selector into a fully-named target before asking for the capability")
	require.Len(t, opts, 1)
	assert.Equal(t, "alice", opts[0].Reviewer)
	assert.True(t, opts[0].RequireUnresolved)

	var payload backend.Report
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	require.Len(t, payload.Reviews, 1)
	assert.Equal(t, "PRR_from_backend", payload.Reviews[0].ID)
	assert.Equal(t, backend.ReportStateApproved, payload.Reviews[0].State)
}

// The built-in gh backend registers the capability, so a pin to it must still
// resolve — and a pin to a backend that does not have it must fail loudly
// rather than produce an empty report.
func TestReviewViewResolvesTheReportCapabilityThroughTheRegistry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name: "built-in backend serves it",
			args: []string{"review", "view", "--backend", "gh", "--repo", "agyn/repo", "51"},
		},
		{
			name:    "a backend without the capability is an error, not an empty report",
			args:    []string{"review", "view", "--backend", "nope", "--repo", "agyn/repo", "51"},
			wantErr: "unknown backend",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeViewAPI{payload: viewResponse, t: t}
			originalFactory := apiClientFactory
			t.Cleanup(func() { apiClientFactory = originalFactory })
			apiClientFactory = func(string) ghcli.API { return fake }

			root := newRootCommand()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(tc.args)

			err := root.Execute()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// GH_HOST reaches the backend as a host, not as whatever the user typed into
// it. resolveTarget is the single place that decision is made, so every verb
// inherits it; before this, threads view passed a raw URL or host:port through
// to the API client while every sibling verb sanitized first.
func TestResolveTargetSanitizesTheHostForEveryVerb(t *testing.T) {
	for _, raw := range []string{
		"https://ghe.example.com",
		"https://ghe.example.com/",
		"ghe.example.com:8443",
		"  ghe.example.com  ",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("GH_HOST", raw)

			_, target, err := resolveTarget(&targetSelector{Repo: "agyn/repo", Pull: 51})
			require.NoError(t, err)
			assert.Equal(t, "ghe.example.com", target.Host)
		})
	}
}

// An unset GH_HOST means github.com, everywhere — not an empty host that
// reaches the gh CLI and is resolved by whatever the ambient config says.
func TestResolveTargetDefaultsTheHostToGitHub(t *testing.T) {
	require.NoError(t, os.Unsetenv("GH_HOST"))

	_, target, err := resolveTarget(&targetSelector{Repo: "agyn/repo", Pull: 51})
	require.NoError(t, err)
	assert.Equal(t, "github.com", target.Host)
}
