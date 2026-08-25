package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRefBaseline(t *testing.T) {
	const fullOID = "3f9c2ab1def0123456789abcdef0123456789ab"

	t.Run("rejects malformed OIDs without calling the API", func(t *testing.T) {
		// A fakeAPI with no graphqlFunc fails the test if the API is reached.
		api := &fakeAPI{}
		for _, raw := range []string{"", "   ", "nothex", "zzzzzzz", "abc g12", "ggggggg"} {
			_, err := ResolveRefBaseline(api, "octo", "demo", raw)
			require.Error(t, err, "raw %q must be rejected", raw)
			assert.Contains(t, err.Error(), "commit OID")
		}
	})

	t.Run("expands a short SHA to the full OID GitHub reports", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Equal(t, "3f9c2ab", variables["oid"])
			return assign(result, CommitQueryResponse{
				Repository: struct {
					Object *CommitObject `json:"object"`
				}{Object: &CommitObject{Oid: fullOID}},
			})
		}}
		status, err := ResolveRefBaseline(api, "octo", "demo", "3f9c2ab")
		require.NoError(t, err)
		require.NotNil(t, status)
		assert.Equal(t, fullOID, status.Oid)
	})

	t.Run("seeds check state from the fetched commit", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, CommitQueryResponse{
				Repository: struct {
					Object *CommitObject `json:"object"`
				}{Object: &CommitObject{
					Oid: fullOID,
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{
						{Conclusion: "FAILURE", App: AppInfo{Name: "CI"}},
					}},
				}},
			})
		}}
		status, err := ResolveRefBaseline(api, "octo", "demo", fullOID)
		require.NoError(t, err)
		assert.Equal(t, []string{"CI"}, status.FailingChecks)
	})

	t.Run("a typo'd or inaccessible SHA fails loudly", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			// No object in the response mirrors FetchCommit's not-found path.
			return assign(result, CommitQueryResponse{})
		}}
		_, err := ResolveRefBaseline(api, "octo", "demo", fullOID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")

		api = &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assert.AnError
		}}
		_, err = ResolveRefBaseline(api, "octo", "demo", fullOID)
		require.ErrorIs(t, err, assert.AnError)
	})
}
