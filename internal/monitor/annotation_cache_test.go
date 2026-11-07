package monitor

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

// annotationServer fakes the two queries a full-tier poll can send: the
// snapshot query, whose runs carry an id and an annotation count but no
// bodies, and the follow-up that reads bodies for the runs it is given.
type annotationServer struct {
	counts    map[string]int // run id -> annotation count the snapshot reports
	snapshots int
	restReads int
	followUps [][]string // ids of each follow-up call
	queries   []string
}

// dbID maps a run's node id ("R7") to its REST id (7).
func dbID(id string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(id, "R"), 10, 64)
	return n
}

func (s *annotationServer) api() *fakeAPI {
	return &fakeAPI{restFunc: func(method, path string, _ map[string]string, _ interface{}, result interface{}) error {
		s.restReads++
		if !strings.Contains(path, "/commits/abc/check-runs") {
			return errors.New("unexpected REST path: " + path)
		}
		runs := []map[string]any{}
		for id, n := range s.counts {
			runs = append(runs, map[string]any{"id": dbID(id), "output": map[string]any{"annotations_count": n}})
		}
		return assign(result, map[string]any{"total_count": len(runs), "check_runs": runs})
	}, graphqlFunc: func(query string, vars map[string]interface{}, result interface{}) error {
		s.queries = append(s.queries, query)
		if strings.Contains(query, "MonitorAnnotations") {
			ids, _ := vars["ids"].([]string)
			s.followUps = append(s.followUps, ids)
			nodes := []map[string]any{}
			for _, id := range ids {
				nodes = append(nodes, map[string]any{
					"id": id,
					"annotations": map[string]any{
						"totalCount": s.counts[id],
						"nodes": []map[string]any{{
							"path": "a.go", "annotationLevel": "NOTICE", "title": "t-" + id, "message": "m",
							"location": map[string]any{"start": map[string]any{"line": 3}},
						}},
					},
				})
			}
			return assign(result, map[string]any{"nodes": nodes})
		}
		s.snapshots++
		runs := []map[string]any{}
		for id := range s.counts {
			runs = append(runs, map[string]any{
				"id": id, "databaseId": dbID(id), "name": "build-" + id, "status": "COMPLETED", "conclusion": "SUCCESS",
			})
		}
		return assign(result, map[string]any{
			"repository": map[string]any{"pullRequest": map[string]any{
				"state": "OPEN",
				"commits": map[string]any{"nodes": []map[string]any{{"commit": map[string]any{
					"oid": "abc",
					"checkSuites": map[string]any{"totalCount": 1, "nodes": []map[string]any{{
						"status": "COMPLETED", "conclusion": "SUCCESS",
						"checkRuns": map[string]any{"nodes": runs},
					}}},
				}}}},
			}},
		})
	}}
}

func annotationTitles(t *testing.T, resp *QueryResponse) []string {
	t.Helper()
	var out []string
	for _, s := range resp.Repository.PullRequest.Commits.Nodes[0].Commit.CheckSuites.Nodes {
		for _, r := range s.CheckRuns.Nodes {
			for _, a := range r.Annotations.Nodes {
				out = append(out, a.Title)
			}
		}
	}
	return out
}

func TestFullTierSnapshotQueryCarriesNoAnnotationBodies(t *testing.T) {
	for name, q := range map[string]string{
		"pr":     MonitorQuery(TierFull),
		"ref":    MonitorRefQuery(TierFull),
		"commit": MonitorCommitQuery(TierFull),
	} {
		assert.NotContains(t, q, "annotations", name, "a nested connection bills once per run")
		assert.Contains(t, q, "databaseId", name)
	}
}

func TestAnnotationFollowUp_UnchangedCountsBuildNoBodyQuery(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}}
	svc := &Service{API: srv.api(), Annotations: NewAnnotationCache()}
	id := &resolver.Identity{Owner: "o", Repo: "r"}

	first, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, []string{"t-R1"}, annotationTitles(t, first))
	require.Len(t, srv.followUps, 1, "a run seen for the first time costs one follow-up")

	second, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 2, srv.snapshots)
	assert.Len(t, srv.followUps, 1, "an unchanged count must not fetch bodies again")
	assert.Equal(t, 2, srv.restReads, "one REST count read per poll")
	assert.Equal(t, []string{"t-R1"}, annotationTitles(t, second), "cached bodies still reach the snapshot")
}

func TestAnnotationFollowUp_ChangedOrNewCountFetchesOnlyThoseRuns(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2, "R2": 1}}
	svc := &Service{API: srv.api(), Annotations: NewAnnotationCache()}
	id := &resolver.Identity{Owner: "o", Repo: "r"}

	_, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	require.Len(t, srv.followUps, 1)

	srv.counts["R1"] = 3 // changed
	srv.counts["R3"] = 1 // new
	_, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	require.Len(t, srv.followUps, 2, "exactly one more follow-up")
	assert.ElementsMatch(t, []string{"R1", "R3"}, srv.followUps[1])
}

func TestAnnotationFollowUp_ZeroCountNeverFetches(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 0}}
	svc := &Service{API: srv.api(), Annotations: NewAnnotationCache()}
	_, err := svc.FetchWithTier(&resolver.Identity{Owner: "o", Repo: "r"}, 7, TierFull)
	require.NoError(t, err)
	assert.Empty(t, srv.followUps)
}

func TestAnnotationFollowUp_ShedTierSkipsEverything(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}}
	svc := &Service{API: srv.api(), Annotations: NewAnnotationCache()}
	_, err := svc.FetchWithTier(&resolver.Identity{Owner: "o", Repo: "r"}, 7, TierNoAnnotations)
	require.NoError(t, err)
	assert.Empty(t, srv.followUps)
	assert.NotContains(t, srv.queries[0], "annotations")
}

func TestAnnotationFollowUp_WithoutCacheStillReportsAnnotations(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}}
	svc := &Service{API: srv.api()}
	resp, err := svc.FetchWithTier(&resolver.Identity{Owner: "o", Repo: "r"}, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, []string{"t-R1"}, annotationTitles(t, resp))
}

func TestAnnotationFollowUp_FailureFailsThePoll(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}}
	api := srv.api()
	inner := api.graphqlFunc
	api.graphqlFunc = func(q string, v map[string]interface{}, r interface{}) error {
		if strings.Contains(q, "MonitorAnnotations") {
			return json.Unmarshal([]byte("{"), &struct{}{})
		}
		return inner(q, v, r)
	}
	svc := &Service{API: api, Annotations: NewAnnotationCache()}
	_, err := svc.FetchWithTier(&resolver.Identity{Owner: "o", Repo: "r"}, 7, TierFull)
	assert.Error(t, err, "a failed body fetch must not read as annotations cleared")
}
