package draft

import (
	"encoding/json"
	"errors"
	"strconv"
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

// graphqlCall records one GraphQL request the service made.
type graphqlCall struct {
	query     string
	variables map[string]interface{}
}

// fakeAPI answers REST calls from a path-keyed table and GraphQL calls from a
// query-substring-keyed table, recording both so a test can assert on what the
// service asked for and not only on what it returned.
type fakeAPI struct {
	restResponses    map[string]func(result interface{}) error
	graphqlResponses map[string]func(variables map[string]interface{}, result interface{}) error

	restCalls    []restCall
	graphqlCalls []graphqlCall
}

func (f *fakeAPI) REST(method, path string, _ map[string]string, _ interface{}, result interface{}) error {
	f.restCalls = append(f.restCalls, restCall{method: method, path: path})
	respond, ok := f.restResponses[path]
	if !ok {
		return errors.New("no stub for path " + path)
	}
	return respond(result)
}

func (f *fakeAPI) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	f.graphqlCalls = append(f.graphqlCalls, graphqlCall{query: query, variables: variables})
	for fragment, respond := range f.graphqlResponses {
		if strings.Contains(query, fragment) {
			return respond(variables, result)
		}
	}
	return errors.New("no stub for query " + query)
}

// restPaths returns the REST paths the service requested, in order.
func (f *fakeAPI) restPaths() []string {
	paths := make([]string, 0, len(f.restCalls))
	for _, call := range f.restCalls {
		paths = append(paths, call.path)
	}
	return paths
}

// queriesContaining returns the GraphQL queries whose text names fragment.
func (f *fakeAPI) queriesContaining(fragment string) []graphqlCall {
	var found []graphqlCall
	for _, call := range f.graphqlCalls {
		if strings.Contains(call.query, fragment) {
			found = append(found, call)
		}
	}
	return found
}

func assign(result interface{}, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

func identity(owner, repo string, number int) resolver.Identity {
	return resolver.Identity{Owner: owner, Repo: repo, Host: "github.com", Number: number, Target: "pr"}
}

// apiForPullRequest stubs the canonicalisation step and the pull request lookup
// the service performs before it touches GraphQL.
func apiForPullRequest(owner, repo, canonical string, number int, nodeID string) *fakeAPI {
	canonicalOwner, canonicalRepo := owner, repo
	if canonical != "" {
		canonicalOwner, canonicalRepo = strings.SplitN(canonical, "/", 2)[0], strings.SplitN(canonical, "/", 2)[1]
	}
	return &fakeAPI{
		restResponses: map[string]func(result interface{}) error{
			"repos/" + owner + "/" + repo: func(result interface{}) error {
				return assign(result, map[string]interface{}{"full_name": canonical})
			},
			"repos/" + canonicalOwner + "/" + canonicalRepo + "/pulls/" + strconv.Itoa(number): func(result interface{}) error {
				return assign(result, map[string]interface{}{
					"node_id": nodeID,
					"head":    map[string]interface{}{"sha": "sha" + strconv.Itoa(number)},
				})
			},
		},
	}
}

func statusResponse(number int, title string, isDraft bool) func(map[string]interface{}, interface{}) error {
	return func(_ map[string]interface{}, result interface{}) error {
		return assign(result, map[string]interface{}{
			"repository": map[string]interface{}{
				"pullRequest": map[string]interface{}{
					"number":  number,
					"title":   title,
					"isDraft": isDraft,
				},
			},
		})
	}
}

func mutationResponse(field string, number int, isDraft bool) func(map[string]interface{}, interface{}) error {
	return func(variables map[string]interface{}, result interface{}) error {
		if _, ok := variables["pullRequestId"]; !ok {
			return errors.New("mutation missing pullRequestId")
		}
		return assign(result, map[string]interface{}{
			field: map[string]interface{}{
				"pullRequest": map[string]interface{}{
					"number":  number,
					"isDraft": isDraft,
				},
			},
		})
	}
}

// The status query must carry the canonical repository, not the one the user
// typed: GraphQL resolves the coordinates as given and would report the pull
// request as missing on a renamed repository.
func TestStatusQueriesCanonicalRepository(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo-org/demo-renamed", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus": statusResponse(5, "Renamed repo", false),
	}

	info, err := NewService(api).Status(identity("octo", "demo", 5), ActionOptions{})

	require.NoError(t, err)
	assert.Equal(t, 5, info.PRNumber)
	assert.Equal(t, "Renamed repo", info.Title)
	assert.False(t, info.IsDraft)

	require.Len(t, api.queriesContaining("PullRequestStatus"), 1)
	assert.Equal(t, map[string]interface{}{
		"owner":  "octo-org",
		"repo":   "demo-renamed",
		"number": 5,
	}, api.queriesContaining("PullRequestStatus")[0].variables)
}

func TestStatusHonoursPRNumberOverride(t *testing.T) {
	// The fixture only answers pulls/7, so honouring the override is the only
	// way this lookup succeeds.
	api := apiForPullRequest("octo", "demo", "octo/demo", 7, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus": statusResponse(7, "Explicit", true),
	}

	info, err := NewService(api).Status(identity("octo", "demo", 5), ActionOptions{PRNumber: 7})

	require.NoError(t, err)
	assert.Equal(t, 7, info.PRNumber)
	assert.Equal(t, []string{"repos/octo/demo", "repos/octo/demo/pulls/7"}, api.restPaths())
	assert.Equal(t, 7, api.queriesContaining("PullRequestStatus")[0].variables["number"])
}

func TestStatusReportsMissingPullRequest(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus": func(_ map[string]interface{}, result interface{}) error {
			return assign(result, map[string]interface{}{
				"repository": map[string]interface{}{"pullRequest": nil},
			})
		},
	}

	_, err := NewService(api).Status(identity("octo", "demo", 5), ActionOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull request 5 not found in octo/demo")
}

// A repository that cannot be resolved stops the service before GraphQL, so a
// renamed repository is reported as unresolvable rather than as a missing pull
// request.
func TestStatusStopsWhenRepositoryCannotBeResolved(t *testing.T) {
	api := &fakeAPI{restResponses: map[string]func(result interface{}) error{
		"repos/octo/demo": func(interface{}) error { return errors.New("HTTP 404") },
	}}

	_, err := NewService(api).Status(identity("octo", "demo", 5), ActionOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository octo/demo not found on github.com")
	assert.Empty(t, api.graphqlCalls)
}

// The node id comes from the lookup, so the draft conversion no longer spends
// a GraphQL round trip resolving it.
func TestDraftConvertsUsingLookupNodeID(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus":         statusResponse(5, "Ready", false),
		"convertPullRequestToDraft": mutationResponse("convertPullRequestToDraft", 5, true),
	}

	result, err := NewService(api).Draft(identity("octo", "demo", 5), ActionOptions{})

	require.NoError(t, err)
	assert.Equal(t, 5, result.PRNumber)
	assert.True(t, result.IsDraft)
	assert.Equal(t, "marked as draft", result.Status)

	assert.Empty(t, api.queriesContaining("PullRequestNodeID"))
	mutation := api.queriesContaining("convertPullRequestToDraft")
	require.Len(t, mutation, 1)
	assert.Equal(t, "PR_kwNode", mutation[0].variables["pullRequestId"])
	// Canonicalisation and the pull request lookup happen once per action.
	assert.Equal(t, []string{"repos/octo/demo", "repos/octo/demo/pulls/5"}, api.restPaths())
}

func TestReadyMarksPullRequestReadyForReview(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus":             statusResponse(5, "Draft", true),
		"markPullRequestReadyForReview": mutationResponse("markPullRequestReadyForReview", 5, false),
	}

	result, err := NewService(api).Ready(identity("octo", "demo", 5), ActionOptions{})

	require.NoError(t, err)
	assert.Equal(t, "marked as ready for review", result.Status)
	assert.False(t, result.IsDraft)
	assert.Equal(t, "PR_kwNode", api.queriesContaining("markPullRequestReadyForReview")[0].variables["pullRequestId"])
}

// Reaching the desired state needs no mutation, and so needs no node id.
func TestAlreadyInDesiredStateSkipsMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		invoke func(*Service) (ActionResult, error)
		state  string
	}{
		{name: "draft", invoke: func(s *Service) (ActionResult, error) { return s.Draft(identity("octo", "demo", 5), ActionOptions{}) }, state: "already draft"},
		{name: "ready", invoke: func(s *Service) (ActionResult, error) { return s.Ready(identity("octo", "demo", 5), ActionOptions{}) }, state: "already ready for review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isDraft := tc.name == "draft"
			api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
			api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
				"PullRequestStatus": statusResponse(5, "Settled", isDraft),
			}

			result, err := tc.invoke(NewService(api))

			require.NoError(t, err)
			assert.Equal(t, tc.state, result.Status)
			assert.Equal(t, isDraft, result.IsDraft)
			assert.Len(t, api.graphqlCalls, 1)
		})
	}
}

// A pull request that cannot be resolved is never mutated.
func TestActionOnMissingPullRequestMakesNoMutation(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus": func(_ map[string]interface{}, result interface{}) error {
			return assign(result, map[string]interface{}{
				"repository": map[string]interface{}{"pullRequest": nil},
			})
		},
	}

	_, err := NewService(api).Draft(identity("octo", "demo", 5), ActionOptions{})

	require.Error(t, err)
	assert.Empty(t, api.queriesContaining("convertPullRequestToDraft"))
}

func TestActionPropagatesMutationFailure(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus": statusResponse(5, "Ready", false),
		"convertPullRequestToDraft": func(_ map[string]interface{}, _ interface{}) error {
			return errors.New("pull request is already a draft")
		},
	}

	_, err := NewService(api).Draft(identity("octo", "demo", 5), ActionOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already a draft")
}

func TestListReturnsDraftsFromCanonicalRepository(t *testing.T) {
	api := &fakeAPI{
		restResponses: map[string]func(result interface{}) error{
			"repos/octo/demo": func(result interface{}) error {
				return assign(result, map[string]interface{}{"full_name": "octo-org/demo-renamed"})
			},
		},
		graphqlResponses: map[string]func(map[string]interface{}, interface{}) error{
			"DraftList": func(_ map[string]interface{}, result interface{}) error {
				return assign(result, map[string]interface{}{
					"repository": map[string]interface{}{
						"pullRequests": map[string]interface{}{
							"nodes": []map[string]interface{}{
								{"number": 3, "title": "Draft PR", "isDraft": true},
								{"number": 4, "title": "Ready PR", "isDraft": false},
								{"number": 5, "title": "Second draft", "isDraft": true},
							},
						},
					},
				})
			},
		},
	}

	drafts, err := NewService(api).List(identity("octo", "demo", 0))

	require.NoError(t, err)
	require.Len(t, drafts, 2)
	assert.Equal(t, 3, drafts[0].PRNumber)
	assert.Equal(t, "Draft PR", drafts[0].Title)
	assert.Equal(t, 5, drafts[1].PRNumber)
	assert.Equal(t, map[string]interface{}{"owner": "octo-org", "repo": "demo-renamed"},
		api.queriesContaining("DraftList")[0].variables)
}

func TestListReportsUnresolvableRepository(t *testing.T) {
	api := &fakeAPI{restResponses: map[string]func(result interface{}) error{
		"repos/octo/demo": func(interface{}) error { return errors.New("HTTP 404") },
	}}

	_, err := NewService(api).List(identity("octo", "demo", 0))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository octo/demo not found on github.com")
	assert.Empty(t, api.graphqlCalls)
}

func TestListReturnsEmptySliceWhenNoPullRequests(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"DraftList": func(_ map[string]interface{}, result interface{}) error {
			return assign(result, map[string]interface{}{
				"repository": map[string]interface{}{"pullRequests": map[string]interface{}{}},
			})
		},
	}

	drafts, err := NewService(api).List(identity("octo", "demo", 0))

	require.NoError(t, err)
	assert.Empty(t, drafts)
	assert.NotNil(t, drafts)
}

func TestServicePropagatesGraphQLErrors(t *testing.T) {
	api := apiForPullRequest("octo", "demo", "octo/demo", 5, "PR_kwNode")
	api.graphqlResponses = map[string]func(map[string]interface{}, interface{}) error{
		"PullRequestStatus": func(_ map[string]interface{}, _ interface{}) error {
			return errors.New("HTTP 502")
		},
	}

	_, err := NewService(api).Status(identity("octo", "demo", 5), ActionOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 502")
}
