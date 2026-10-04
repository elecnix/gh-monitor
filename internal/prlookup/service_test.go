package prlookup

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restCall records one REST request the service made.
type restCall struct {
	method string
	path   string
}

// fakeAPI answers REST calls from a path-keyed table and records every call,
// so a test can assert on the request sequence and not just the result.
type fakeAPI struct {
	responses map[string]func(result interface{}) error
	calls     []restCall
}

func (f *fakeAPI) REST(method, path string, _ map[string]string, _ interface{}, result interface{}) error {
	f.calls = append(f.calls, restCall{method: method, path: path})
	respond, ok := f.responses[path]
	if !ok {
		return errors.New("no stub for path " + path)
	}
	return respond(result)
}

func (f *fakeAPI) GraphQL(string, map[string]interface{}, interface{}) error {
	return errors.New("unexpected GraphQL call")
}

func (f *fakeAPI) paths() []string {
	paths := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		paths = append(paths, call.path)
	}
	return paths
}

func assign(result interface{}, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// repositoryOK stubs `GET repos/<owner>/<repo>` with a full_name, the field
// GitHub rewrites when an owner or repository is renamed.
func repositoryOK(fullName string) func(interface{}) error {
	return func(result interface{}) error {
		return assign(result, map[string]interface{}{"full_name": fullName})
	}
}

// assignRaw unmarshals a literal API payload into the caller's result.
func assignRaw(result interface{}, payload string) error {
	return json.Unmarshal([]byte(payload), result)
}

// pullOK stubs `GET repos/<path>/pulls/<n>` with a pull request payload.
func pullOK(nodeID, headSHA string) func(interface{}) error {
	return func(result interface{}) error {
		return assign(result, map[string]interface{}{
			"node_id": nodeID,
			"head":    map[string]interface{}{"sha": headSHA},
		})
	}
}

func TestPullRequestReturnsNodeIDHeadSHAAndRequestedNumber(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo":         repositoryOK("octo/demo"),
		"repos/octo/demo/pulls/5": pullOK("PR_kwNode", "abc123"),
	}}

	ref, err := NewService(api).PullRequest(resolver.Identity{
		Owner:  "octo",
		Repo:   "demo",
		Host:   "github.com",
		Number: 5,
		Target: "pr",
	})

	require.NoError(t, err)
	assert.Equal(t, "PR_kwNode", ref.NodeID)
	assert.Equal(t, "abc123", ref.HeadSHA)
	assert.Equal(t, 5, ref.Number)
	assert.Equal(t, "octo", ref.Owner)
	assert.Equal(t, "demo", ref.Name)
	assert.Equal(t, "github.com", ref.Host)
}

// A renamed owner/repository still resolves: REST follows the redirect and
// reports the canonical full_name, which the pull request request must use.
func TestPullRequestFollowsRepositoryRename(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo":                     repositoryOK("octo-org/demo-renamed"),
		"repos/octo-org/demo-renamed/pulls/5": pullOK("PR_kwNode", "abc123"),
	}}

	ref, err := NewService(api).PullRequest(resolver.Identity{
		Owner:  "octo",
		Repo:   "demo",
		Host:   "github.com",
		Number: 5,
	})

	require.NoError(t, err)
	assert.Equal(t, "octo-org", ref.Owner)
	assert.Equal(t, "demo-renamed", ref.Name)
	assert.Equal(t, []string{"repos/octo/demo", "repos/octo-org/demo-renamed/pulls/5"}, api.paths())
}

func TestPullRequestRefIdentityRoundTrips(t *testing.T) {
	ref := Ref{Repository: Repository{Host: "github.com", Owner: "octo", Name: "demo"}, Number: 5, NodeID: "PR_kwNode", HeadSHA: "abc123"}

	assert.Equal(t, resolver.Identity{
		Owner:  "octo",
		Repo:   "demo",
		Host:   "github.com",
		Number: 5,
		Target: "pr",
	}, ref.Identity())
}

func TestPullRequestReportsUnresolvableRepository(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo": func(interface{}) error {
			return errors.New("HTTP 404")
		},
	}}

	_, err := NewService(api).PullRequest(resolver.Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 5})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository octo/demo not found on github.com")
	assert.Contains(t, err.Error(), "HTTP 404")
	// The pull request request must not be attempted against an unknown repository.
	assert.Equal(t, []string{"repos/octo/demo"}, api.paths())
}

func TestPullRequestReportsMissingPullRequest(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo":         repositoryOK("octo/demo"),
		"repos/octo/demo/pulls/5": func(interface{}) error { return errors.New("HTTP 404") },
	}}

	_, err := NewService(api).PullRequest(resolver.Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 5})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull request 5 not found in octo/demo")
	assert.Contains(t, err.Error(), "HTTP 404")
}

// The canonical name is the one worth reporting, not the one the user typed.
func TestPullRequestNotFoundNamesCanonicalRepository(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo":                     repositoryOK("octo-org/demo-renamed"),
		"repos/octo-org/demo-renamed/pulls/9": func(interface{}) error { return errors.New("HTTP 404") },
	}}

	_, err := NewService(api).PullRequest(resolver.Identity{Owner: "octo", Repo: "demo", Host: "github.com", Number: 9})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull request 9 not found in octo-org/demo-renamed")
}

func TestPullRequestRejectsIncompleteMetadata(t *testing.T) {
	for name, payload := range map[string]string{
		"missing node id":  `{"head":{"sha":"abc123"}}`,
		"blank node id":    `{"node_id":"   ","head":{"sha":"abc123"}}`,
		"missing head sha": `{"node_id":"PR_kwNode"}`,
		"blank head sha":   `{"node_id":"PR_kwNode","head":{"sha":""}}`,
		"missing head":     `{"node_id":"PR_kwNode"}`,
		"empty response":   `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{responses: map[string]func(interface{}) error{
				"repos/octo/demo": repositoryOK("octo/demo"),
				"repos/octo/demo/pulls/5": func(result interface{}) error {
					return assignRaw(result, payload)
				},
			}}

			_, err := NewService(api).PullRequest(resolver.Identity{Owner: "octo", Repo: "demo", Number: 5})

			require.Error(t, err)
			assert.Contains(t, err.Error(), "pull request 5 missing")
		})
	}
}

func TestRepositoryReturnsCanonicalCoordinates(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo": repositoryOK("octo-org/demo-renamed"),
	}}

	repo, err := NewService(api).Repository(resolver.Identity{Owner: "octo", Repo: "demo", Host: "github.com"})

	require.NoError(t, err)
	assert.Equal(t, Repository{Owner: "octo-org", Name: "demo-renamed", Host: "github.com"}, repo)
	assert.Equal(t, []string{"repos/octo/demo"}, api.paths())
}

func TestRepositoryIdentityRoundTrips(t *testing.T) {
	repo := Repository{Owner: "octo-org", Name: "demo-renamed", Host: "github.com"}

	assert.Equal(t, resolver.Identity{Owner: "octo-org", Repo: "demo-renamed", Host: "github.com"}, repo.Identity())
}

// A full_name that is absent or not exactly `owner/repo` must not rewrite the
// requested coordinates: half of one would point at a different repository.
func TestRepositoryKeepsRequestedCoordinatesWhenFullNameIsUnusable(t *testing.T) {
	for name, fullName := range map[string]string{
		"absent":      "",
		"no slash":    "demo",
		"empty owner": "/demo",
		"empty name":  "octo/",
		"nested":      "octo/team/demo",
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{responses: map[string]func(interface{}) error{
				"repos/octo/demo": repositoryOK(fullName),
			}}

			repo, err := NewService(api).Repository(resolver.Identity{Owner: "octo", Repo: "demo", Host: "github.com"})

			require.NoError(t, err)
			assert.Equal(t, "octo", repo.Owner)
			assert.Equal(t, "demo", repo.Name)
		})
	}
}

func TestRepositoryReportsMissingRepository(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo": func(interface{}) error { return errors.New("HTTP 404") },
	}}

	_, err := NewService(api).Repository(resolver.Identity{Owner: "octo", Repo: "demo", Host: "github.com"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository octo/demo not found on github.com")
}

func TestLookupUsesGETOnly(t *testing.T) {
	api := &fakeAPI{responses: map[string]func(interface{}) error{
		"repos/octo/demo":         repositoryOK("octo/demo"),
		"repos/octo/demo/pulls/5": pullOK("PR_kwNode", "abc123"),
	}}

	_, err := NewService(api).PullRequest(resolver.Identity{Owner: "octo", Repo: "demo", Number: 5})
	require.NoError(t, err)

	require.Len(t, api.calls, 2)
	for _, call := range api.calls {
		assert.Equal(t, "GET", call.method)
		assert.False(t, strings.HasPrefix(call.path, "/"), "path must be relative to the host: "+call.path)
	}
}
