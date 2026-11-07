package monitor

import (
	"fmt"
	"sync"
)

// annotationBatch caps how many runs one follow-up query names. GitHub accepts
// at most 100 ids in a nodes() lookup.
const annotationBatch = 100

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

// AnnotationCache stores the annotation bodies read for each check run, per
// watched target. It is safe for concurrent use.
type AnnotationCache struct {
	mu      sync.Mutex
	targets map[string]map[string]cachedAnnotations
}

// NewAnnotationCache returns an empty cache.
func NewAnnotationCache() *AnnotationCache {
	return &AnnotationCache{targets: map[string]map[string]cachedAnnotations{}}
}

func stampOf(r *CheckRun) annotationStamp {
	return annotationStamp{r.Annotations.TotalCount, r.Status, r.Conclusion, r.CompletedAt}
}

// annotationCounts reads every check run's annotation count for a commit with
// one REST list (per 100 runs), keyed by the run's database id. REST has its
// own quota, so the count costs the GraphQL budget nothing. It is skipped when
// no run carries a database id.
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
// annotations. The snapshot query reports no counts, so they come from REST and the bodies come
// from the cache when the run's stamp is unchanged and from one follow-up
// query (per 100 runs) otherwise. A failed follow-up fails the poll: returning
// the snapshot without bodies would read as annotations cleared.
func (s *Service) fillAnnotations(target, owner, repo, oid string, suites []CheckSuite) error {
	counts, err := s.annotationCounts(owner, repo, oid, suites)
	if err != nil {
		return err
	}
	var prev map[string]cachedAnnotations
	if s.Annotations != nil {
		s.Annotations.mu.Lock()
		prev = s.Annotations.targets[target]
		s.Annotations.mu.Unlock()
	}
	next := map[string]cachedAnnotations{}
	var stale []string
	var staleRuns []*CheckRun
	for i := range suites {
		runs := suites[i].CheckRuns.Nodes
		for j := range runs {
			run := &runs[j]
			run.Annotations.TotalCount = counts[run.DatabaseID]
			if run.ID == "" || run.Annotations.TotalCount == 0 {
				continue
			}
			stamp := stampOf(run)
			if c, ok := prev[run.ID]; ok && c.stamp == stamp {
				run.Annotations = c.set
				next[run.ID] = c
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
			next[run.ID] = cachedAnnotations{stamp: stamp, set: set}
		}
	}
	if s.Annotations != nil {
		s.Annotations.mu.Lock()
		s.Annotations.targets[target] = next
		s.Annotations.mu.Unlock()
	}
	return nil
}
