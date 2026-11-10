package monitor

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// annotationBatch caps how many runs one follow-up query names. GitHub accepts
// at most 100 ids in a nodes() lookup.
const annotationBatch = 100

// AnnotationCountTTL is how long a target's annotation counts are served from
// the cache while its check runs are unchanged. A run can gain annotations
// after it completed, even a passing one, so a count read once cannot stand
// for the rest of the watch. With the TTL an idle target costs one REST call
// per five minutes instead of one per poll, and a late annotation reaches a
// consumer within five minutes.
const AnnotationCountTTL = 5 * time.Minute

const monitorAnnotationsQuery = `query MonitorAnnotations($ids: [ID!]!) {
  nodes(ids: $ids) {
    ... on CheckRun {
      id
      annotations(first: 50) {
        totalCount
        nodes { path location { start { line } } annotationLevel title message }
      }
    }
  }
}`

// annotationStamp identifies the state of a check run an annotation read
// belongs to. A run that changes status or conclusion, or whose count moves,
// is read again.
type annotationStamp struct {
	count                         int
	status, conclusion, completed string
}

type cachedAnnotations struct {
	stamp annotationStamp
	set   AnnotationNodes
}

// targetAnnotations is what the cache remembers about one watched target: the
// bodies read per run, and the last REST count read with the check-run state
// it was read against.
type targetAnnotations struct {
	runs map[string]cachedAnnotations
	// counts is the last count read, by database id. countsAt is when it was
	// read and runsState the check-run state it describes.
	counts    map[int64]int
	runsState string
	countsAt  time.Time
}

// AnnotationCache stores the annotation bodies and counts read for each
// watched target. It is safe for concurrent use.
type AnnotationCache struct {
	mu      sync.Mutex
	targets map[string]targetAnnotations
	// now is the clock the count TTL is measured on. Tests move it by hand.
	now func() time.Time
}

// NewAnnotationCache returns an empty cache.
func NewAnnotationCache() *AnnotationCache {
	return &AnnotationCache{targets: map[string]targetAnnotations{}, now: time.Now}
}

func (c *AnnotationCache) get(target string) targetAnnotations {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targets[target]
}

func (c *AnnotationCache) put(target string, t targetAnnotations) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets[target] = t
}

func stampOf(r *CheckRun) annotationStamp {
	return annotationStamp{r.Annotations.TotalCount, r.Status, r.Conclusion, r.CompletedAt}
}

// runsState summarises the check runs a snapshot carries: the commit, and
// each run's id, status, conclusion and completion time. A count read is
// reused only while this is unchanged. Annotation counts are deliberately not
// part of it: they are what the read produces, so they cannot decide whether
// to read.
func runsState(oid string, suites []CheckSuite) string {
	var lines []string
	for i := range suites {
		for _, r := range suites[i].CheckRuns.Nodes {
			lines = append(lines, strings.Join([]string{r.ID, r.Status, r.Conclusion, r.CompletedAt}, "\x1f"))
		}
	}
	sort.Strings(lines)
	return oid + "\n" + strings.Join(lines, "\n")
}

// annotationCounts reads every check run's annotation count for a commit with
// one REST list (per 100 runs), keyed by the run's database id. REST has its
// own quota, so the count costs the GraphQL budget nothing. It is skipped when
// no run carries a database id. fillAnnotations decides when to call it.
func (s *Service) annotationCounts(owner, repo, oid string, suites []CheckSuite) (map[int64]int, error) {
	counts := map[int64]int{}
	need := false
	for i := range suites {
		for j := range suites[i].CheckRuns.Nodes {
			if suites[i].CheckRuns.Nodes[j].DatabaseID != 0 {
				need = true
			}
		}
	}
	if !need || oid == "" {
		return counts, nil
	}
	read := 0
	for page := 1; page <= maxCheckPages; page++ {
		var resp struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				ID     int64 `json:"id"`
				Output struct {
					AnnotationsCount int `json:"annotations_count"`
				} `json:"output"`
			} `json:"check_runs"`
		}
		path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", owner, repo, oid, page)
		if err := s.API.REST("GET", path, nil, nil, &resp); err != nil {
			return nil, fmt.Errorf("read check-run annotation counts over REST: %w", err)
		}
		for _, r := range resp.CheckRuns {
			counts[r.ID] = r.Output.AnnotationsCount
		}
		read += len(resp.CheckRuns)
		if len(resp.CheckRuns) == 0 || read >= resp.TotalCount {
			break
		}
	}
	return counts, nil
}

// fillAnnotations puts annotation bodies on every run of suites that has
// annotations. The snapshot query reports no counts, so they come from REST:
// a fresh read when a run appeared, changed state or completed, or when the
// last read is older than AnnotationCountTTL, and the cached counts
// otherwise. A count read that fails serves the cached counts and keeps the
// poll alive: a spent REST budget must not read as a monitoring outage. The
// bodies come from the cache when the run's stamp is unchanged and from one
// follow-up query (per 100 runs) otherwise. A failed follow-up fails the
// poll: returning the snapshot without bodies would read as annotations
// cleared.
func (s *Service) fillAnnotations(target, owner, repo, oid string, suites []CheckSuite) error {
	var prev targetAnnotations
	now := time.Now()
	if s.Annotations != nil {
		prev = s.Annotations.get(target)
		now = s.Annotations.now()
	}
	next := prev
	next.runs = map[string]cachedAnnotations{}
	if state := runsState(oid, suites); state != prev.runsState || now.Sub(prev.countsAt) >= AnnotationCountTTL {
		// A failed read records nothing, so the next poll reads again.
		if counts, err := s.annotationCounts(owner, repo, oid, suites); err == nil {
			next.counts, next.runsState, next.countsAt = counts, state, now
		}
	}
	var stale []string
	var staleRuns []*CheckRun
	for i := range suites {
		runs := suites[i].CheckRuns.Nodes
		for j := range runs {
			run := &runs[j]
			run.Annotations.TotalCount = next.counts[run.DatabaseID]
			if run.ID == "" || run.Annotations.TotalCount == 0 {
				continue
			}
			stamp := stampOf(run)
			if c, ok := prev.runs[run.ID]; ok && c.stamp == stamp {
				run.Annotations = c.set
				next.runs[run.ID] = c
				continue
			}
			stale = append(stale, run.ID)
			staleRuns = append(staleRuns, run)
		}
	}
	for start := 0; start < len(stale); start += annotationBatch {
		end := min(start+annotationBatch, len(stale))
		var resp struct {
			Nodes []struct {
				ID          string          `json:"id"`
				Annotations AnnotationNodes `json:"annotations"`
			} `json:"nodes"`
		}
		err := s.API.GraphQL(monitorAnnotationsQuery, map[string]interface{}{"ids": stale[start:end]}, &resp)
		if err != nil {
			return fmt.Errorf("reading check-run annotations: %w", err)
		}
		bodies := map[string]AnnotationNodes{}
		for _, n := range resp.Nodes {
			bodies[n.ID] = n.Annotations
		}
		for _, run := range staleRuns[start:end] {
			set, ok := bodies[run.ID]
			if !ok {
				return fmt.Errorf("reading check-run annotations: run %s missing from the response", run.ID)
			}
			stamp := stampOf(run)
			run.Annotations = set
			next.runs[run.ID] = cachedAnnotations{stamp: stamp, set: set}
		}
	}
	if s.Annotations != nil {
		s.Annotations.put(target, next)
	}
	return nil
}
