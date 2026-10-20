package hub

import (
	"context"
	"testing"
	"time"

	"github.com/elecnix/gh-monitor/internal/monitor"
	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// annotatedPRFixture builds a PR whose single check suite carries two
// completed check runs, each with one warning-level annotation: the check a
// consumer watches, and the noisy one that reports the same warning from
// every workflow job.
func annotatedPRFixture() *monitor.PullRequest {
	ann := func(path, title, message string) monitor.Annotation {
		a := monitor.Annotation{Path: path, Level: "WARNING", Title: title, Message: message}
		a.Location.Start.Line = 12
		return a
	}
	suite := monitor.CheckSuite{
		Status:     "COMPLETED",
		Conclusion: "SUCCESS",
		App:        monitor.AppInfo{Name: "CI"},
		CheckRuns: monitor.RunNodes{Nodes: []monitor.CheckRun{
			{
				Name:        "dupdelta",
				Status:      "COMPLETED",
				Conclusion:  "SUCCESS",
				Annotations: monitor.AnnotationNodes{TotalCount: 1, Nodes: []monitor.Annotation{ann("src/dup.go", "duplication", "clone of src/orig.go")}},
			},
			{
				Name:        "lint",
				Status:      "COMPLETED",
				Conclusion:  "SUCCESS",
				Annotations: monitor.AnnotationNodes{TotalCount: 1, Nodes: []monitor.Annotation{ann("src/x.go", "deprecated API", "Foo is deprecated")}},
			},
		}},
	}
	return &monitor.PullRequest{
		State: "OPEN",
		Commits: monitor.CommitNodes{Nodes: []monitor.Commit{{Commit: monitor.CommitDetails{
			Oid:         "aaaaaaa",
			CheckSuites: monitor.SuiteNodes{Nodes: []monitor.CheckSuite{suite}},
		}}}},
	}
}

// TestHub_AnnotationChecksFilterCrossesTheProcessBoundary confirms the
// per-check-name filter survives the trip through WatchOptions — the wire
// the daemon and an out-of-process backend see (issue #110).
//
// Without the filter crossing the boundary, the daemon would apply no
// check-name filter at all and every check's annotation would reach the
// consumer, which is the noise the filter exists to remove.
func TestHub_AnnotationChecksFilterCrossesTheProcessBoundary(t *testing.T) {
	snap := annotatedPRFixture()
	h := New(func(ctx context.Context, _ resolver.Identity, _ monitor.QueryTier) (any, error) {
		return snap, nil
	}, nil, time.Hour, nil)
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	t.Run("a named check reaches the consumer, the other does not", func(t *testing.T) {
		opts := testHubOpts()
		opts.AnnotationChecks = []string{"dupdelta"}

		ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), opts)
		t.Cleanup(cancelSub)

		names := collectAnnotations(ch, 2*time.Second)
		require.NotEmpty(t, names, "the named check's annotation must reach the consumer")
		for _, n := range names {
			assert.Equal(t, "dupdelta", n, "the check-name filter must cross the WatchOptions boundary")
		}
	})

	t.Run("none drops every annotation", func(t *testing.T) {
		opts := testHubOpts()
		opts.AnnotationChecks = []string{"none"}

		ch, cancelSub := h.SubscribePR(ctx, testHubTarget(), opts)
		t.Cleanup(cancelSub)

		// The first-poll notice still arrives, so the stream is not silent;
		// what must never arrive is an annotation event.
		assert.Nil(t, collectAnnotations(ch, 500*time.Millisecond),
			"\"none\" must drop every annotation")
	})

	t.Run("the one-shot read applies the filter too", func(t *testing.T) {
		// The one-shot path is what an agent's probe uses, so a filter
		// that only worked on the watch path would miss the case the
		// issue names. The assertion is on the annotation's CONTENT rather
		// than its check name, so it says something the watch-path
		// assertion does not.
		opts := testHubOpts()
		opts.Once = true
		opts.AnnotationChecks = []string{"dupdelta"}

		ch := h.Once(ctx, testHubTarget(), opts)
		got := collectAnnotation(ch, 2*time.Second)
		require.NotNil(t, got, "the one-shot read must deliver the named check's annotation")
		assert.Equal(t, "clone of src/orig.go", got.Message)
		assert.Equal(t, 12, got.Line)
	})
}
