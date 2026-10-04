package gh

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #140: the hub receives the distilled status, not the wire payload.
// For a workflow run the distillation reads no per-subscriber snapshot
// options, so it happens once here — at the only place that holds the REST
// payload — and never again downstream. These tests pin that, so the
// distillation cannot quietly slide back out to the hub.

// fakeAPI answers the two ghcli methods with canned bodies.
type fakeAPI struct {
	rest    func(method, path string, params map[string]string, body, result interface{}) error
	graphql func(query string, variables map[string]interface{}, result interface{}) error
}

func (f *fakeAPI) REST(method, path string, params map[string]string, body, result interface{}) error {
	if f.rest == nil {
		return errors.New("unexpected REST call")
	}
	return f.rest(method, path, params, body, result)
}

func (f *fakeAPI) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	if f.graphql == nil {
		return errors.New("unexpected GraphQL call")
	}
	return f.graphql(query, variables, result)
}

// assign decodes payload into result the way the gh CLI client does.
func assign(result, payload interface{}) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, result)
}

// runRESTBody mirrors the fields of a real workflow-run response. Terminal
// states are the ones the API actually reports — a blank status/conclusion
// pair does not exist (see AGENTS.md).
func runRESTBody(status, conclusion string) map[string]interface{} {
	return map[string]interface{}{
		"id":            30433642,
		"name":          "deploy",
		"display_title": "Deploy to prod",
		"event":         "workflow_dispatch",
		"status":        status,
		"conclusion":    conclusion,
		"head_branch":   "main",
		"head_sha":      "abcdef1234567890",
		"html_url":      "https://github.com/octo/demo/actions/runs/30433642",
		"run_number":    42,
	}
}

func runAPI(t *testing.T, body map[string]interface{}, err error) func(host string) ghcli.API {
	t.Helper()
	return func(string) ghcli.API {
		return &fakeAPI{rest: func(method, path string, _ map[string]string, _, result interface{}) error {
			if err != nil {
				return err
			}
			assert.Equal(t, "GET", method)
			assert.Equal(t, "repos/octo/demo/actions/runs/30433642", path)
			return assign(result, body)
		}}
	}
}

func runIdentity() resolver.Identity {
	return resolver.Identity{Target: "run", Owner: "octo", Repo: "demo", RunID: 30433642, Host: "github.com"}
}

// TestFetchRunShipsAStatusNotAPayload is the acceptance test for issue #140:
// what crosses into the hub for a run is a *monitor.RunStatus. If it ever
// goes back to *monitor.WorkflowRun, this fails — the hub would then be the
// only holder of a payload it would have to re-distil per subscriber.
func TestFetchRunShipsAStatusNotAPayload(t *testing.T) {
	got, err := Fetch(runAPI(t, runRESTBody("completed", "failure"), nil))(context.Background(), runIdentity(), monitor.TierFull)
	require.NoError(t, err)

	st, ok := got.(*monitor.RunStatus)
	require.True(t, ok, "a run fetch must return *monitor.RunStatus, got %T", got)
	assert.Equal(t, backend.KindRun, st.TargetKind())
	assert.Equal(t, 30433642, st.RunID)
	assert.Equal(t, "deploy", st.Name)
	assert.Equal(t, "Deploy to prod", st.DisplayTitle)
	assert.Equal(t, "workflow_dispatch", st.Event)
	assert.Equal(t, "completed", st.Status)
	assert.Equal(t, "failure", st.Conclusion)
	assert.Equal(t, "main", st.HeadBranch)
	assert.Equal(t, "abcdef1234567890", st.HeadSHA)
	assert.Equal(t, "abcdef1", st.ShortSHA, "distilled once, here — not by the receiver")
	assert.Equal(t, 42, st.RunNumber)
}

// TestFetchRunStillDistilsAnUnfinishedRun: an in-flight run has no conclusion
// at all, and that empty string is what the status must carry.
func TestFetchRunStillDistillsAnUnfinishedRun(t *testing.T) {
	got, err := Fetch(runAPI(t, runRESTBody("in_progress", ""), nil))(context.Background(), runIdentity(), monitor.TierFull)
	require.NoError(t, err)
	st, ok := got.(*monitor.RunStatus)
	require.True(t, ok)
	assert.Equal(t, "in_progress", st.Status)
	assert.Empty(t, st.Conclusion)
	assert.False(t, st.IsTerminal())
}

// TestFetchRunPropagatesError: a failed read must surface as an error, never
// as a zero-valued status the hub would read as "all clear".
func TestFetchRunPropagatesError(t *testing.T) {
	boom := errors.New("boom")
	got, err := Fetch(runAPI(t, nil, boom))(context.Background(), runIdentity(), monitor.TierFull)
	require.ErrorIs(t, err, boom)
	assert.Nil(t, got)
}

// TestReadRunShipsTheSameShape keeps the two entry points in step: a
// one-shot read and the daemon's fetch must produce the same status for the
// same run, whichever path a caller reaches the backend through.
func TestReadRunShipsTheSameShape(t *testing.T) {
	p := &Provider{API: runAPI(t, runRESTBody("completed", "success"), nil)}
	got, err := p.read(context.Background(), backend.Target{
		Kind: backend.KindRun, Owner: "octo", Repo: "demo", RunID: 30433642, Host: "github.com",
	})
	require.NoError(t, err)

	viaRead, ok := got.(*monitor.RunStatus)
	require.True(t, ok)
	viaFetch, err := Fetch(p.API)(context.Background(), runIdentity(), monitor.TierFull)
	require.NoError(t, err)
	assert.Equal(t, viaFetch, viaRead)
	assert.Equal(t, "success", viaRead.Conclusion)
}
