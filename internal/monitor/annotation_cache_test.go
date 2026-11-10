package monitor

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// annotationServer fakes the two queries a full-tier poll can send: the
// snapshot query, whose runs carry an id and an annotation count but no
// bodies, and the follow-up that reads bodies for the runs it is given.
type annotationServer struct {
	counts    map[string]int    // run id -> annotation count the REST list reports
	status    map[string]string // run id -> status; COMPLETED when unset
	completed map[string]string // run id -> completedAt; empty when unset
	restErr   error             // returned by every REST read when set
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
		if s.restErr != nil {
			return s.restErr
		}
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
			status := s.status[id]
			if status == "" {
				status = "COMPLETED"
			}
			runs = append(runs, map[string]any{
				"id": id, "databaseId": dbID(id), "name": "build-" + id, "status": status, "conclusion": "SUCCESS",
				"completedAt": s.completed[id],
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
	assert.Equal(t, 1, srv.restReads, "an idle poll inside the TTL reads no counts over REST")
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

// tickingCache returns a cache whose clock the test moves by hand.
func tickingCache() (*AnnotationCache, *time.Time) {
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	cache := NewAnnotationCache()
	cache.now = func() time.Time { return clock }
	return cache, &clock
}

func TestAnnotationCounts_IdlePollInsideTTLReadsNothingOverREST(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}}
	cache, clock := tickingCache()
	svc := &Service{API: srv.api(), Annotations: cache}
	id := &resolver.Identity{Owner: "o", Repo: "r"}

	_, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	require.Equal(t, 1, srv.restReads, "a run seen for the first time is read at once")

	for i := 0; i < 4; i++ {
		*clock = clock.Add(time.Minute)
		resp, err := svc.FetchWithTier(id, 7, TierFull)
		require.NoError(t, err)
		assert.Equal(t, []string{"t-R1"}, annotationTitles(t, resp), "cached counts and bodies still reach the snapshot")
	}
	assert.Equal(t, 1, srv.restReads, "four idle polls inside the TTL cost no REST call")
	assert.Len(t, srv.followUps, 1)
}

func TestAnnotationCounts_RunChangeReadsCountsOnce(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}, status: map[string]string{"R1": "IN_PROGRESS"}}
	cache, clock := tickingCache()
	svc := &Service{API: srv.api(), Annotations: cache}
	id := &resolver.Identity{Owner: "o", Repo: "r"}

	_, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	require.Equal(t, 1, srv.restReads)

	*clock = clock.Add(time.Second)
	srv.status["R1"] = "COMPLETED"
	_, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 2, srv.restReads, "a status change reads the counts once")

	*clock = clock.Add(time.Second)
	srv.completed = map[string]string{"R1": "2026-11-10T12:00:02Z"}
	_, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 3, srv.restReads, "a completedAt change reads the counts once")

	*clock = clock.Add(time.Second)
	srv.counts["R2"] = 0
	_, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 4, srv.restReads, "a run that appears for the first time is read at once")

	*clock = clock.Add(time.Second)
	_, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 4, srv.restReads, "the next idle poll reads nothing")
}

func TestAnnotationCounts_LateAnnotationIsReadAfterTTL(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 0}}
	cache, clock := tickingCache()
	svc := &Service{API: srv.api(), Annotations: cache}
	id := &resolver.Identity{Owner: "o", Repo: "r"}

	resp, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Empty(t, annotationTitles(t, resp))
	require.Equal(t, 1, srv.restReads)

	// An annotation added to a passing run after it completed: no run field
	// changes, so only the TTL can expose it.
	srv.counts["R1"] = 2
	*clock = clock.Add(AnnotationCountTTL - time.Second)
	resp, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 1, srv.restReads, "inside the TTL the cached count stands")
	assert.Empty(t, annotationTitles(t, resp))

	*clock = clock.Add(time.Second)
	resp, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 2, srv.restReads, "the TTL expiring reads the counts once")
	assert.Equal(t, []string{"t-R1"}, annotationTitles(t, resp))
	assert.Len(t, srv.followUps, 1)
}

func TestAnnotationCounts_RESTFailureServesCachedCounts(t *testing.T) {
	for name, restErr := range map[string]error{
		"transport":     errors.New("dial tcp: connection refused"),
		"403 exhausted": &ghcli.APIError{StatusCode: 403, Message: "API rate limit exceeded"},
		"429":           &ghcli.APIError{StatusCode: 429, Message: "too many requests"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &annotationServer{counts: map[string]int{"R1": 2}}
			cache, clock := tickingCache()
			svc := &Service{API: srv.api(), Annotations: cache}
			id := &resolver.Identity{Owner: "o", Repo: "r"}

			_, err := svc.FetchWithTier(id, 7, TierFull)
			require.NoError(t, err)

			srv.restErr = restErr
			*clock = clock.Add(AnnotationCountTTL)
			resp, err := svc.FetchWithTier(id, 7, TierFull)
			require.NoError(t, err, "a failed count read must not fail the poll")
			assert.Equal(t, 2, srv.restReads, "the read was attempted")
			assert.Equal(t, []string{"t-R1"}, annotationTitles(t, resp), "the cached counts and bodies stand")
			assert.Len(t, srv.followUps, 1, "cached counts need no body fetch")

			// A failed read leaves nothing to cache, so the next poll reads again.
			srv.restErr = nil
			*clock = clock.Add(time.Second)
			resp, err = svc.FetchWithTier(id, 7, TierFull)
			require.NoError(t, err)
			assert.Equal(t, 3, srv.restReads, "the read is retried once REST answers")
			assert.Equal(t, []string{"t-R1"}, annotationTitles(t, resp))
		})
	}
}

func TestAnnotationCounts_RESTFailureOnFirstPollKeepsThePollAlive(t *testing.T) {
	srv := &annotationServer{counts: map[string]int{"R1": 2}, restErr: &ghcli.APIError{StatusCode: 403}}
	cache, clock := tickingCache()
	svc := &Service{API: srv.api(), Annotations: cache}
	id := &resolver.Identity{Owner: "o", Repo: "r"}

	resp, err := svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err, "a spent REST budget is not a monitoring outage")
	assert.Empty(t, annotationTitles(t, resp), "nothing is known yet")
	assert.Empty(t, srv.followUps)

	srv.restErr = nil
	*clock = clock.Add(time.Second)
	resp, err = svc.FetchWithTier(id, 7, TierFull)
	require.NoError(t, err)
	assert.Equal(t, 2, srv.restReads)
	assert.Equal(t, []string{"t-R1"}, annotationTitles(t, resp))
}
