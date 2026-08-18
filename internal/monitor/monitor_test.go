package monitor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/internal/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is an injectable ghcli.API for tests.
type fakeAPI struct {
	graphqlFunc func(query string, variables map[string]interface{}, result interface{}) error
	restFunc    func(method, path string, params map[string]string, body interface{}, result interface{}) error
}

func (f *fakeAPI) REST(method, path string, params map[string]string, body interface{}, result interface{}) error {
	if f.restFunc == nil {
		return errors.New("unexpected REST call")
	}
	return f.restFunc(method, path, params, body, result)
}

func (f *fakeAPI) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	if f.graphqlFunc == nil {
		return errors.New("unexpected GraphQL call")
	}
	return f.graphqlFunc(query, variables, result)
}

func assign(result interface{}, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

// thumbsUp returns reaction groups acknowledging a comment.
func thumbsUp() []ReactionGroup {
	return []ReactionGroup{{Content: "THUMBS_UP", Users: struct {
		TotalCount int `json:"totalCount"`
	}{TotalCount: 1}}}
}

func ptr(i int) *int { return &i }

func TestFetch(t *testing.T) {
	t.Run("sends query and unmarshals", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Contains(t, query, "MonitorPR")
			assert.Equal(t, "octocat", variables["owner"])
			assert.Equal(t, "hello", variables["repo"])
			assert.Equal(t, 7, variables["number"])
			return assign(result, QueryResponse{
				Repository: struct {
					PullRequest *PullRequest `json:"pullRequest"`
				}{PullRequest: &PullRequest{State: "OPEN"}},
			})
		}}
		svc := &Service{API: api}
		got, err := svc.Fetch(&resolver.Identity{Owner: "octocat", Repo: "hello"}, 7)
		require.NoError(t, err)
		assert.Equal(t, "OPEN", got.Repository.PullRequest.State)
	})

	t.Run("nil PR is an error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, QueryResponse{})
		}}
		svc := &Service{API: api}
		_, err := svc.Fetch(&resolver.Identity{}, 1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("propagates API error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return errors.New("boom")
		}}
		svc := &Service{API: api}
		_, err := svc.Fetch(&resolver.Identity{}, 1)
		require.Error(t, err)
	})
}

func TestSnapshot_Basic(t *testing.T) {
	pr := &PullRequest{
		State:     "OPEN",
		Merged:    false,
		Mergeable: "MERGEABLE",
		Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
			Oid:             "abcdef1234567890",
			MessageHeadline: "fix things",
		}}}},
	}
	s := Snapshot(pr, SnapshotOptions{})
	assert.Equal(t, "OPEN", s.State)
	assert.False(t, s.Merged)
	assert.False(t, s.Conflict)
	assert.Empty(t, s.UnresolvedThreads)
	assert.Empty(t, s.GeneralComments)
	assert.Equal(t, "abcdef1", s.LastCommit.ShortOid)
	assert.Equal(t, "abcdef1234567890", s.LastCommit.Oid)
	assert.Equal(t, "fix things", s.LastCommit.MessageHeadline)
}

func TestSnapshot_Conflict(t *testing.T) {
	s := Snapshot(&PullRequest{Mergeable: "CONFLICTING"}, SnapshotOptions{})
	assert.True(t, s.Conflict)
}

func TestSnapshot_ThreadFiltering(t *testing.T) {
	pr := &PullRequest{ReviewThreads: ThreadNodes{Nodes: []ReviewThread{
		// unresolved, unacked -> included
		{ID: "T1", IsResolved: false, Path: "a.go", Line: ptr(10), Comments: CommentNodes{Nodes: []Comment{
			{ID: "C1", Body: "please fix"},
		}}},
		// resolved -> excluded
		{ID: "T2", IsResolved: true, Comments: CommentNodes{Nodes: []Comment{{ID: "C2"}}}},
		// unresolved but last comment acked -> excluded
		{ID: "T3", IsResolved: false, Comments: CommentNodes{Nodes: []Comment{
			{ID: "C3a", Body: "nit"},
			{ID: "C3b", Body: "done", ReactionGroups: thumbsUp()},
		}}},
	}}}
	s := Snapshot(pr, SnapshotOptions{})
	require.Len(t, s.UnresolvedThreads, 1)
	assert.Equal(t, "T1", s.UnresolvedThreads[0].ID)
	assert.Equal(t, "a.go", s.UnresolvedThreads[0].Path)
	assert.Equal(t, 10, *s.UnresolvedThreads[0].Line)
	assert.Equal(t, []string{"C1"}, s.UnresolvedThreads[0].CommentIDs)
}

func TestSnapshot_GeneralCommentFiltering(t *testing.T) {
	pr := &PullRequest{Comments: CommentNodes{Nodes: []Comment{
		mkComment("C1", "alice", "hey", nil),
		mkComment("C2", "bob", "acked", thumbsUp()), // acked -> excluded
		mkComment("C3", "dependabot", "bump dep", nil),
	}}}

	t.Run("ack filtering", func(t *testing.T) {
		s := Snapshot(pr, SnapshotOptions{})
		require.Len(t, s.GeneralComments, 2)
		assert.Equal(t, "C1", s.GeneralComments[0].ID)
		assert.Equal(t, "alice", s.GeneralComments[0].Author)
		assert.Equal(t, "C3", s.GeneralComments[1].ID)
	})

	t.Run("ignored bots filtering", func(t *testing.T) {
		s := Snapshot(pr, SnapshotOptions{IgnoredBots: []string{"dependabot"}})
		require.Len(t, s.GeneralComments, 1)
		assert.Equal(t, "C1", s.GeneralComments[0].ID)
	})
}

func mkComment(id, author, body string, reactions []ReactionGroup) Comment {
	c := Comment{ID: id, Body: body, ReactionGroups: reactions}
	c.Author.Login = author
	return c
}

func TestSnapshot_FailingAndPendingChecks(t *testing.T) {
	pr := &PullRequest{Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
		CheckSuites: SuiteNodes{Nodes: []CheckSuite{
			{Conclusion: "FAILURE", App: AppInfo{Name: "CI"}},
			{Status: "IN_PROGRESS", App: AppInfo{Name: "Deploy"}},
			{CheckRuns: RunNodes{Nodes: []CheckRun{{Name: "unit", Conclusion: "ERROR"}}}},
		}},
		Status: &CommitStatus{Contexts: []StatusContext{
			{State: "FAILURE", Context: "circleci"},
			{State: "PENDING", Context: "buildkite"},
		}},
	}}}}}
	s := Snapshot(pr, SnapshotOptions{})
	assert.ElementsMatch(t, []string{"CI", "unit", "circleci"}, s.FailingChecks)
	assert.ElementsMatch(t, []string{"Deploy", "buildkite"}, s.PendingChecks)
}

func TestSnapshot_PendingCoversEveryNonTerminalStatus(t *testing.T) {
	// GitHub's CheckStatusState: COMPLETED plus these five. Anything that is
	// not COMPLETED is still in flight and must land in PendingChecks — a suite
	// filed as neither failing nor pending reads as green.
	for _, status := range []string{"REQUESTED", "PENDING", "QUEUED", "WAITING", "IN_PROGRESS"} {
		t.Run(status, func(t *testing.T) {
			pr := &PullRequest{Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
				CheckSuites: SuiteNodes{Nodes: []CheckSuite{{Status: status, App: AppInfo{Name: "CI"}}}},
			}}}}}
			s := Snapshot(pr, SnapshotOptions{})
			assert.Equal(t, []string{"CI"}, s.PendingChecks)
			assert.Empty(t, s.SuccessfulChecks)
		})
	}
}

func TestSnapshot_SuccessfulChecks(t *testing.T) {
	pr := &PullRequest{Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
		CheckSuites: SuiteNodes{Nodes: []CheckSuite{
			{Status: "COMPLETED", Conclusion: "SUCCESS", App: AppInfo{Name: "CI"}},
			{Status: "COMPLETED", Conclusion: "SKIPPED", App: AppInfo{Name: "Optional"}},
			{Status: "COMPLETED", Conclusion: "FAILURE", App: AppInfo{Name: "Deploy"}},
			{Status: "COMPLETED", CheckRuns: RunNodes{Nodes: []CheckRun{
				{Name: "unit", Conclusion: "SUCCESS"},
				{Name: "lint", Conclusion: "NEUTRAL"},
				{Name: "e2e", Conclusion: "TIMED_OUT"},
			}}},
		}},
		Status: &CommitStatus{Contexts: []StatusContext{
			{State: "SUCCESS", Context: "circleci"},
			{State: "FAILURE", Context: "buildkite"},
		}},
	}}}}}
	s := Snapshot(pr, SnapshotOptions{})
	assert.ElementsMatch(t, []string{"CI", "Optional", "unit", "lint", "circleci"}, s.SuccessfulChecks)
	assert.ElementsMatch(t, []string{"Deploy", "e2e", "buildkite"}, s.FailingChecks)
	assert.Empty(t, s.PendingChecks)
}

func TestSnapshot_NoChecksMeansNoSuccess(t *testing.T) {
	s := Snapshot(&PullRequest{Commits: CommitNodes{Nodes: []Commit{{}}}}, SnapshotOptions{})
	assert.Empty(t, s.SuccessfulChecks)
	assert.Empty(t, s.FailingChecks)
	assert.Empty(t, s.PendingChecks)
}

func TestSnapshot_CheckAnnotations(t *testing.T) {
	mkAnn := func(path, level, title, message string, line int) Annotation {
		return Annotation{
			Path:    path,
			Level:   level,
			Title:   title,
			Message: message,
			Location: AnnotationLocation{
				Start: struct {
					Line int `json:"line"`
				}{Line: line},
			},
		}
	}

	t.Run("extracts warning annotation", func(t *testing.T) {
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "advisory"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:       "warn-only",
							Conclusion: "SUCCESS",
							Status:     "COMPLETED",
							Annotations: AnnotationNodes{Nodes: []Annotation{
								mkAnn("src/main.go", "WARNING", "deprecated API", "Foo is deprecated, use Bar", 42),
							}},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		require.Len(t, s.CheckAnnotations, 1)
		a := s.CheckAnnotations[0]
		assert.Equal(t, "warn-only", a.CheckName)
		assert.Equal(t, "src/main.go", a.Path)
		assert.Equal(t, 42, a.Line)
		assert.Equal(t, "WARNING", a.Level)
	})

	t.Run("filters notice-level annotations", func(t *testing.T) {
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "CI"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:       "build",
							Conclusion: "SUCCESS",
							Status:     "COMPLETED",
							Annotations: AnnotationNodes{Nodes: []Annotation{
								mkAnn(".github/cache", "NOTICE", "cache miss", "...", 0),
								mkAnn("src/x.go", "WARNING", "lint", "unused var", 5),
							}},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		require.Len(t, s.CheckAnnotations, 1)
		assert.Equal(t, "WARNING", s.CheckAnnotations[0].Level)
	})

	t.Run("empty when no annotations", func(t *testing.T) {
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid:         "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{Status: "COMPLETED", Conclusion: "SUCCESS", App: AppInfo{Name: "CI"}}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Empty(t, s.CheckAnnotations)
	})

	t.Run("notice included when AnnotationLevels allows it", func(t *testing.T) {
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "CI"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:       "build",
							Conclusion: "SUCCESS",
							Status:     "COMPLETED",
							Annotations: AnnotationNodes{Nodes: []Annotation{
								mkAnn("cache", "NOTICE", "cache miss", "...", 0),
								mkAnn("src/x.go", "WARNING", "lint", "unused var", 5),
							}},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{AnnotationLevels: NewAnnotationLevels("notice", "warning", "failure")})
		require.Len(t, s.CheckAnnotations, 2)
		levels := make([]string, len(s.CheckAnnotations))
		for i, a := range s.CheckAnnotations {
			levels[i] = a.Level
		}
		assert.Contains(t, levels, "NOTICE")
		assert.Contains(t, levels, "WARNING")
	})

	t.Run("notice still dropped by default", func(t *testing.T) {
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "CI"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:       "build",
							Conclusion: "SUCCESS",
							Status:     "COMPLETED",
							Annotations: AnnotationNodes{Nodes: []Annotation{
								mkAnn("cache", "NOTICE", "cache miss", "...", 0),
								mkAnn("src/x.go", "WARNING", "lint", "unused var", 5),
							}},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{}) // nil AnnotationLevels = default (warning+failure)
		require.Len(t, s.CheckAnnotations, 1)
		assert.Equal(t, "WARNING", s.CheckAnnotations[0].Level)
	})

	t.Run("none drops all annotation levels", func(t *testing.T) {
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "CI"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:       "build",
							Conclusion: "SUCCESS",
							Status:     "COMPLETED",
							Annotations: AnnotationNodes{Nodes: []Annotation{
								mkAnn("src/x.go", "WARNING", "lint", "unused var", 5),
								mkAnn("src/y.go", "FAILURE", "security", "CVE", 10),
							}},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{AnnotationLevels: NewAnnotationLevels()}) // empty = none
		assert.Empty(t, s.CheckAnnotations)
	})
}

func TestSnapshot_CheckAnnotations_Truncation(t *testing.T) {
	mkAnn := func(path, level, title, message string, line int) Annotation {
		return Annotation{
			Path:    path,
			Level:   level,
			Title:   title,
			Message: message,
			Location: AnnotationLocation{
				Start: struct {
					Line int `json:"line"`
				}{Line: line},
			},
		}
	}

	// Build a slice of N annotations (all WARNING).
	mkAnns := func(n int) []Annotation {
		anns := make([]Annotation, n)
		for i := 0; i < n; i++ {
			anns[i] = mkAnn("src/file.go", "WARNING", "lint", "unused var", i+1)
		}
		return anns
	}

	t.Run("exactly 10 annotations on a check run flags truncation", func(t *testing.T) {
		anns := mkAnns(10)
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "lint"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:        "lint",
							Conclusion:  "SUCCESS",
							Status:      "COMPLETED",
							Permalink:   "https://github.com/o/r/actions/runs/123",
							Annotations: AnnotationNodes{TotalCount: 10, Nodes: anns},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.True(t, s.AnnotationsTruncated, "10 annotations (at the per-step cap) must flag truncation")
		assert.Equal(t, "https://github.com/o/r/actions/runs/123", s.AnnotationsURL)
		require.Len(t, s.CheckAnnotations, 10)
	})

	t.Run("fewer than 10 annotations does not flag truncation", func(t *testing.T) {
		anns := mkAnns(3)
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "lint"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:        "lint",
							Conclusion:  "SUCCESS",
							Status:      "COMPLETED",
							Annotations: AnnotationNodes{TotalCount: 3, Nodes: anns},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.False(t, s.AnnotationsTruncated, "fewer than 10 annotations must NOT flag truncation")
		assert.Empty(t, s.AnnotationsURL)
		require.Len(t, s.CheckAnnotations, 3)
	})

	t.Run("totalCount exceeding fetched count flags truncation (page full)", func(t *testing.T) {
		// totalCount=55 but only 50 nodes returned — page is full.
		anns := mkAnns(50)
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "scanner"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:        "scanner",
							Conclusion:  "SUCCESS",
							Status:      "COMPLETED",
							Permalink:   "https://github.com/o/r/actions/runs/456",
							Annotations: AnnotationNodes{TotalCount: 55, Nodes: anns},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.True(t, s.AnnotationsTruncated, "totalCount > fetched must flag truncation")
		assert.Equal(t, "https://github.com/o/r/actions/runs/456", s.AnnotationsURL)
		require.Len(t, s.CheckAnnotations, 50)
	})

	t.Run("totalCount equals fetched count does not flag page truncation", func(t *testing.T) {
		anns := mkAnns(5)
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "lint"},
						CheckRuns: RunNodes{Nodes: []CheckRun{{
							Name:        "lint",
							Conclusion:  "SUCCESS",
							Status:      "COMPLETED",
							Annotations: AnnotationNodes{TotalCount: 5, Nodes: anns},
						}}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.False(t, s.AnnotationsTruncated, "totalCount == fetched must NOT flag truncation")
	})

	t.Run("second run crossing the 10 cap sets URL from first", func(t *testing.T) {
		// First run is under 10, second hits 10 — URL is from the first hit.
		pr := &PullRequest{
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid: "abc",
					CheckSuites: SuiteNodes{Nodes: []CheckSuite{{
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
						App:        AppInfo{Name: "CI"},
						CheckRuns: RunNodes{Nodes: []CheckRun{
							{
								Name:        "lint",
								Conclusion:  "SUCCESS",
								Status:      "COMPLETED",
								Permalink:   "https://github.com/o/r/actions/runs/1",
								Annotations: AnnotationNodes{TotalCount: 10, Nodes: mkAnns(10)},
							},
							{
								Name:        "scanner",
								Conclusion:  "SUCCESS",
								Status:      "COMPLETED",
								Permalink:   "https://github.com/o/r/actions/runs/2",
								Annotations: AnnotationNodes{TotalCount: 12, Nodes: mkAnns(12)},
							},
						}},
					}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.True(t, s.AnnotationsTruncated)
		// URL from first truncating run.
		assert.Equal(t, "https://github.com/o/r/actions/runs/1", s.AnnotationsURL)
		require.Len(t, s.CheckAnnotations, 22)
	})
}

func TestSnapshot_ReviewDecision(t *testing.T) {
	t.Run("latest non-pending", func(t *testing.T) {
		pr := &PullRequest{Reviews: ReviewNodes{Nodes: []Review{mkReview("APPROVED", "carol")}}}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Equal(t, "APPROVED", s.ReviewDecision)
		assert.Equal(t, "carol", s.ReviewAuthor)
	})
	t.Run("pending ignored", func(t *testing.T) {
		pr := &PullRequest{Reviews: ReviewNodes{Nodes: []Review{mkReview("PENDING", "dave")}}}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Equal(t, "", s.ReviewDecision)
		assert.Equal(t, "", s.ReviewAuthor)
	})
	t.Run("no reviews", func(t *testing.T) {
		s := Snapshot(&PullRequest{}, SnapshotOptions{})
		assert.Equal(t, "", s.ReviewDecision)
	})
	t.Run("later COMMENTED review does not clobber the approval", func(t *testing.T) {
		// A reviewer approves and then leaves a follow-up COMMENTED review.
		// COMMENTED is not a decision, so the APPROVED decision must survive
		// and stay attributed to the approver.
		pr := &PullRequest{Reviews: ReviewNodes{Nodes: []Review{
			mkReview("APPROVED", "carol"),
			mkReview("COMMENTED", "carol"),
		}}}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Equal(t, "APPROVED", s.ReviewDecision)
		assert.Equal(t, "carol", s.ReviewAuthor)
	})
	t.Run("later COMMENTED review by another reviewer does not clobber the approval", func(t *testing.T) {
		pr := &PullRequest{Reviews: ReviewNodes{Nodes: []Review{
			mkReview("APPROVED", "carol"),
			mkReview("COMMENTED", "dave"),
		}}}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Equal(t, "APPROVED", s.ReviewDecision)
		assert.Equal(t, "carol", s.ReviewAuthor)
	})
	t.Run("commented-only review is not a decision", func(t *testing.T) {
		pr := &PullRequest{Reviews: ReviewNodes{Nodes: []Review{
			mkReview("COMMENTED", "erin"),
		}}}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Equal(t, "", s.ReviewDecision)
		assert.Equal(t, "", s.ReviewAuthor)
	})
	t.Run("changes requested then later approval wins", func(t *testing.T) {
		pr := &PullRequest{Reviews: ReviewNodes{Nodes: []Review{
			mkReview("CHANGES_REQUESTED", "carol"),
			mkReview("COMMENTED", "carol"),
			mkReview("APPROVED", "carol"),
		}}}
		s := Snapshot(pr, SnapshotOptions{})
		assert.Equal(t, "APPROVED", s.ReviewDecision)
		assert.Equal(t, "carol", s.ReviewAuthor)
	})
}

func mkReview(state, author string) Review {
	r := Review{State: state}
	r.Author.Login = author
	return r
}

func TestSnapshot_LastCommitAuthorAndCoauthors(t *testing.T) {
	pr := &PullRequest{Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
		Oid:             "0123456789abcdef",
		MessageHeadline: "feat: add thing",
		Message:         "feat: add thing\n\nBody.\n\nCo-authored-by: Ada Lovelace <ada@example.com>\nCo-authored-by: Alan Turing <alan@example.com>\n",
		Authors: GitActorNodes{Nodes: []GitActor{
			{Name: "Grace Hopper", User: &struct {
				Login string `json:"login"`
			}{Login: "grace"}},
		}},
	}}}}}
	s := Snapshot(pr, SnapshotOptions{})
	assert.Equal(t, "grace", s.LastCommit.Author)
	assert.Equal(t, "0123456", s.LastCommit.ShortOid)
	assert.Equal(t, []string{"Ada Lovelace", "Alan Turing"}, s.LastCommit.Coauthors)
}

func TestSnapshot_AuthorFallbackToName(t *testing.T) {
	pr := &PullRequest{Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
		Oid:     "aaaa",
		Authors: GitActorNodes{Nodes: []GitActor{{Name: "No Account"}}},
	}}}}}
	s := Snapshot(pr, SnapshotOptions{})
	assert.Equal(t, "No Account", s.LastCommit.Author)
	assert.Equal(t, "aaaa", s.LastCommit.ShortOid) // shorter than 7, unchanged
}

func TestParseCoauthors(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{"empty", "", nil},
		{"none", "just a message", nil},
		{"single", "msg\n\nCo-authored-by: Ada <ada@x.com>", []string{"Ada"}},
		{"case insensitive", "co-AUTHORED-by: Bob <b@x.com>", []string{"Bob"}},
		{"strips email keeps name", "Co-authored-by: Jane Doe <jane@example.com>", []string{"Jane Doe"}},
		{"no email", "Co-authored-by: Nameonly", []string{"Nameonly"}},
		{"dedup", "Co-authored-by: Ada <a@x>\nCo-authored-by: Ada <a@x>", []string{"Ada"}},
		{"order", "Co-authored-by: B <b@x>\nCo-authored-by: A <a@x>", []string{"B", "A"}},
		{"leading whitespace", "   Co-authored-by:   Spaced   <s@x>  ", []string{"Spaced"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseCoauthors(tt.message))
		})
	}
}

func TestIsAcknowledged(t *testing.T) {
	assert.True(t, isAcknowledged(&Comment{ReactionGroups: thumbsUp()}))
	assert.False(t, isAcknowledged(&Comment{}))
	// group present but zero users -> not acknowledged
	assert.False(t, isAcknowledged(&Comment{ReactionGroups: []ReactionGroup{{Content: "THUMBS_UP"}}}))
	// other reaction -> not acknowledged
	assert.False(t, isAcknowledged(&Comment{ReactionGroups: []ReactionGroup{{Content: "HEART", Users: struct {
		TotalCount int `json:"totalCount"`
	}{TotalCount: 3}}}}))
}

func TestSnapshot_JSONRoundTrip(t *testing.T) {
	s := Snapshot(&PullRequest{State: "OPEN", Mergeable: "MERGEABLE"}, SnapshotOptions{})
	data, err := json.Marshal(s)
	require.NoError(t, err)
	// nil-safe empty arrays
	assert.True(t, strings.Contains(string(data), `"unresolved_threads":[]`))
	assert.True(t, strings.Contains(string(data), `"general_comments":[]`))
}

// ---------------------------------------------------------------------------
// Ref target tests
// ---------------------------------------------------------------------------

func TestFetchRef(t *testing.T) {
	t.Run("sends query and unmarshals", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Contains(t, query, "MonitorRef")
			assert.Equal(t, "octocat", variables["owner"])
			assert.Equal(t, "hello", variables["repo"])
			assert.Equal(t, "main", variables["ref"])
			return assign(result, RefQueryResponse{
				Repository: struct {
					Ref *RefTarget `json:"ref"`
				}{Ref: &RefTarget{Target: RefTarget{}.Target}},
			})
		}}
		svc := &Service{API: api}
		got, err := svc.FetchRef("octocat", "hello", "main")
		require.NoError(t, err)
		assert.Equal(t, "", got.Repository.Ref.Target.Oid)
	})

	t.Run("nil ref is an error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, RefQueryResponse{})
		}}
		svc := &Service{API: api}
		_, err := svc.FetchRef("o", "r", "nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("propagates API error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return errors.New("boom")
		}}
		svc := &Service{API: api}
		_, err := svc.FetchRef("o", "r", "main")
		require.Error(t, err)
	})
}

func TestFetchCommit(t *testing.T) {
	t.Run("sends query and unmarshals", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Contains(t, query, "MonitorCommit")
			assert.Equal(t, "octocat", variables["owner"])
			assert.Equal(t, "hello", variables["repo"])
			assert.Equal(t, "abc123", variables["oid"])
			return assign(result, CommitQueryResponse{
				Repository: struct {
					Object *CommitObject `json:"object"`
				}{Object: &CommitObject{Oid: "abc123"}},
			})
		}}
		svc := &Service{API: api}
		got, err := svc.FetchCommit("octocat", "hello", "abc123")
		require.NoError(t, err)
		assert.Equal(t, "abc123", got.Repository.Object.Oid)
	})

	t.Run("nil object is an error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, CommitQueryResponse{})
		}}
		svc := &Service{API: api}
		_, err := svc.FetchCommit("o", "r", "abc")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestSnapshotRef_Checks(t *testing.T) {
	rt := RefTarget{}
	rt.Target.Oid = "abc123"

	// No checks data.
	s := SnapshotRef(&rt)
	assert.Equal(t, "abc123", s.Oid)
	assert.Empty(t, s.FailingChecks)
	assert.Empty(t, s.PendingChecks)
}

func TestSnapshotRef_WithCommitDetails(t *testing.T) {
	rt := RefTarget{}
	rt.Target.Oid = "def456"
	rt.Target.MessageHeadline = "fix: stuff"
	rt.Target.Authors = GitActorNodes{Nodes: []GitActor{{Name: "Grace", User: &struct {
		Login string `json:"login"`
	}{Login: "grace"}}}}
	rt.Target.CheckSuites = SuiteNodes{Nodes: []CheckSuite{
		{Conclusion: "FAILURE", App: AppInfo{Name: "CI"}},
		{Status: "IN_PROGRESS", App: AppInfo{Name: "Deploy"}},
	}}
	rt.Target.Status = &CommitStatus{Contexts: []StatusContext{
		{State: "FAILURE", Context: "circleci"},
	}}

	s := SnapshotRef(&rt)
	assert.Equal(t, "def456", s.Oid)
	assert.Equal(t, "def456", s.ShortOid)
	assert.ElementsMatch(t, []string{"CI", "circleci"}, s.FailingChecks)
	assert.ElementsMatch(t, []string{"Deploy"}, s.PendingChecks)
	assert.Equal(t, "grace", s.Author)
	assert.Equal(t, "fix: stuff", s.MessageHeadline)
}

// ---------------------------------------------------------------------------
// Issue target tests
// ---------------------------------------------------------------------------

func mkIssueComment(id, author, body string, reacted bool) IssueComment {
	c := IssueComment{ID: id, Body: body}
	c.Author.Login = author
	if reacted {
		c.ReactionGroups = thumbsUp()
	}
	return c
}

func TestFetchIssue(t *testing.T) {
	t.Run("sends query and unmarshals", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Contains(t, query, "MonitorIssue")
			assert.Equal(t, "octocat", variables["owner"])
			assert.Equal(t, "hello", variables["repo"])
			assert.Equal(t, 42, variables["number"])
			return assign(result, IssueQueryResponse{
				Repository: struct {
					Issue *IssueNode `json:"issue"`
				}{Issue: &IssueNode{State: "OPEN"}},
			})
		}}
		svc := &Service{API: api}
		got, err := svc.FetchIssue("octocat", "hello", 42)
		require.NoError(t, err)
		assert.Equal(t, "OPEN", got.Repository.Issue.State)
	})

	t.Run("nil issue is an error", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			return assign(result, IssueQueryResponse{})
		}}
		svc := &Service{API: api}
		_, err := svc.FetchIssue("o", "r", 1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestSnapshotIssue_Basic(t *testing.T) {
	issue := &IssueNode{State: "OPEN", Title: "bug report"}
	s := SnapshotIssue(issue, SnapshotOptions{})
	assert.Equal(t, "OPEN", s.State)
	assert.Equal(t, "bug report", s.Title)
	assert.Empty(t, s.Comments)
}

func TestSnapshotIssue_CommentFiltering(t *testing.T) {
	issue := &IssueNode{State: "OPEN", Comments: IssueCommentNodes{Nodes: []IssueComment{
		mkIssueComment("C1", "alice", "hey", false),
		mkIssueComment("C2", "bob", "done", true), // acked -> excluded
		mkIssueComment("C3", "bot", "automated", false),
	}}}

	t.Run("ack filtering", func(t *testing.T) {
		s := SnapshotIssue(issue, SnapshotOptions{})
		require.Len(t, s.Comments, 2)
		assert.Equal(t, "C1", s.Comments[0].ID)
		assert.Equal(t, "C3", s.Comments[1].ID)
	})

	t.Run("ignored bots", func(t *testing.T) {
		s := SnapshotIssue(issue, SnapshotOptions{IgnoredBots: []string{"bot"}})
		require.Len(t, s.Comments, 1)
		assert.Equal(t, "C1", s.Comments[0].ID)
	})
}

func TestDiffIssues_StateChanges(t *testing.T) {
	t.Run("open to closed", func(t *testing.T) {
		prev := &IssueStatus{State: "OPEN"}
		curr := &IssueStatus{State: "CLOSED"}
		events := DiffIssues(prev, curr)
		require.Len(t, events, 1)
		assert.Equal(t, EventIssueClosed, events[0].Type)
	})

	t.Run("closed to open (reopened)", func(t *testing.T) {
		prev := &IssueStatus{State: "CLOSED"}
		curr := &IssueStatus{State: "OPEN"}
		events := DiffIssues(prev, curr)
		require.Len(t, events, 1)
		assert.Equal(t, EventIssueReopened, events[0].Type)
	})

	t.Run("no state change", func(t *testing.T) {
		prev := &IssueStatus{State: "OPEN"}
		curr := &IssueStatus{State: "OPEN"}
		events := DiffIssues(prev, curr)
		assert.Empty(t, events)
	})
}

func TestDiffIssues_NewComments(t *testing.T) {
	prev := &IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "C1"}}}
	curr := &IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{
		{ID: "C1", Author: "a", Body: "old"},
		{ID: "C2", Author: "b", Body: "new"},
	}}
	events := DiffIssues(prev, curr)
	require.Len(t, events, 1)
	assert.Equal(t, EventIssueNewComment, events[0].Type)
	require.Len(t, events[0].IssueComments, 1)
	assert.Equal(t, "C2", events[0].IssueComments[0].ID)
}

func TestDiffIssues_NilPrev(t *testing.T) {
	curr := &IssueStatus{State: "OPEN", Comments: []IssueCommentSummary{{ID: "C1"}}}
	events := DiffIssues(nil, curr)
	assert.Empty(t, events)
}

// ---------------------------------------------------------------------------
// Ruleset / required checks tests (issue #30)
// ---------------------------------------------------------------------------

func mkPRWithCheckSuites(suites ...CheckSuite) *PullRequest {
	return &PullRequest{
		State: "OPEN",
		Commits: CommitNodes{Nodes: []Commit{{
			Commit: CommitDetails{
				Oid:             "abc1234",
				MessageHeadline: "fix",
				CheckSuites:     SuiteNodes{Nodes: suites, TotalCount: len(suites)},
			},
		}}},
	}
}

func TestSnapshot_AwaitingChecks(t *testing.T) {
	// A required context that is entirely absent from the payload must appear
	// in AwaitingChecks — it is not failing (it never ran), not pending.
	t.Run("absent required check reported as awaiting", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"}},
		)
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Contexts: []string{"CI", "security-scan"}},
		})
		assert.ElementsMatch(t, []string{"security-scan"}, s.AwaitingChecks)
		assert.Empty(t, s.FailingChecks)
		assert.Empty(t, s.PendingChecks)
		assert.Equal(t, "", s.RulesetError)
	})

	t.Run("all required checks present — no awaiting", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"}},
		)
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Contexts: []string{"CI"}},
		})
		assert.Empty(t, s.AwaitingChecks)
	})

	t.Run("required check present as a run name", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{
				Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"},
				CheckRuns: RunNodes{Nodes: []CheckRun{{Name: "unit", Conclusion: "SUCCESS"}}},
			},
		)
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Contexts: []string{"unit"}},
		})
		assert.Empty(t, s.AwaitingChecks)
		assert.Contains(t, s.SuccessfulChecks, "unit")
	})

	t.Run("required check present as status context", func(t *testing.T) {
		pr := &PullRequest{
			State: "OPEN",
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid:             "abc1234",
					MessageHeadline: "fix",
					Status:          &CommitStatus{Contexts: []StatusContext{{State: "SUCCESS", Context: "circleci"}}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Contexts: []string{"circleci"}},
		})
		assert.Empty(t, s.AwaitingChecks)
	})

	t.Run("no ruleset — no awaiting", func(t *testing.T) {
		pr := mkPRWithCheckSuites()
		s := Snapshot(pr, SnapshotOptions{}) // nil RulesetChecks
		assert.Empty(t, s.AwaitingChecks)
		assert.Equal(t, "", s.RulesetError)
	})

	t.Run("empty required list — no awaiting", func(t *testing.T) {
		pr := mkPRWithCheckSuites()
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Contexts: nil},
		})
		assert.Empty(t, s.AwaitingChecks)
	})
}

func TestSnapshot_RulesetError(t *testing.T) {
	// A ruleset that cannot be read must produce a loud degraded signal — a
	// non-empty RulesetError — rather than an empty required-set that silently
	// becomes "nothing is required."
	t.Run("ruleset error is surfaced", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"}},
		)
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Error: "cannot read ruleset: HTTP 403"},
		})
		assert.Equal(t, "cannot read ruleset: HTTP 403", s.RulesetError)
		assert.Empty(t, s.AwaitingChecks)
	})

	t.Run("ciAllGreen false when ruleset has error", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"}},
		)
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Error: "cannot read ruleset"},
		})
		assert.False(t, ciAllGreen(s), "ci-all-green must be false when ruleset cannot be read")
	})

	t.Run("ciAllGreen false when awaiting", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"}},
		)
		s := Snapshot(pr, SnapshotOptions{
			RulesetChecks: &RulesetChecks{Contexts: []string{"CI", "missing-check"}},
		})
		assert.False(t, ciAllGreen(s), "ci-all-green must be false when a required check is awaiting")
	})
}

func TestSnapshot_CancelledBesideSuccessIsNotFailing(t *testing.T) {
	// Measured 2026-08-18 on a live PR: every name carrying a cancelled or skipped run
	// ALSO carried a successful one, and the event reported them all as FAILING. The
	// classification must be PER NAME ACROSS ALL RUNS, and the NON-VERDICT row here is
	// the NEWER one (00:18:22 vs 00:18:01) — ordering by time alone would yield
	// "cancelled", which is not the answer either. The full rule: a non-verdict never
	// overrides a verdict, whichever is newer.
	pr := mkPRWithCheckSuites(
		CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: AppInfo{Name: "terraform-test"}},
	)
	pr.Commits.Nodes[0].Commit.CheckSuites.Nodes[0].CheckRuns = RunNodes{Nodes: []CheckRun{
		{Name: "terraform-test", Status: "COMPLETED", Conclusion: "SUCCESS",
			StartedAt: "2026-08-18T00:18:01Z", CompletedAt: "2026-08-18T00:18:01Z"},
		{Name: "terraform-test", Status: "COMPLETED", Conclusion: "CANCELLED",
			StartedAt: "2026-08-18T00:18:22Z", CompletedAt: "2026-08-18T00:18:22Z"},
	}}
	s := Snapshot(pr, SnapshotOptions{
		RulesetChecks: &RulesetChecks{Contexts: []string{"terraform-test"}},
	})
	assert.NotContains(t, s.FailingChecks, "terraform-test", "a newer cancelled beside a success is NOT a failure")
	assert.Contains(t, s.SuccessfulChecks, "terraform-test")
	assert.False(t, s.Conflict)
	assert.NotEmpty(t, s.SuccessfulChecks, "the success must still be counted as positive evidence")
}

func TestSnapshot_LatestVerdictWinsAmongVerdicts(t *testing.T) {
	// The second half of the rule, learned the hard way on a live PR: the `review`
	// check had TWO conclusive runs on one head — success at 02:45, FAILURE at 02:47
	// (the re-run after the reviewer posted its findings). "Any success wins" is the
	// mirror of the laundered-red trap: it launders a red with an earlier green.
	pr := mkPRWithCheckSuites(CheckSuite{Status: "COMPLETED", App: AppInfo{Name: "review"}})
	pr.Commits.Nodes[0].Commit.CheckSuites.Nodes[0].CheckRuns = RunNodes{Nodes: []CheckRun{
		{Name: "review", Status: "COMPLETED", Conclusion: "SUCCESS",
			StartedAt: "2026-08-18T02:45:20Z", CompletedAt: "2026-08-18T02:45:29Z"},
		{Name: "review", Status: "COMPLETED", Conclusion: "FAILURE",
			StartedAt: "2026-08-18T02:47:26Z", CompletedAt: "2026-08-18T02:47:36Z"},
	}}
	s := Snapshot(pr, SnapshotOptions{
		RulesetChecks: &RulesetChecks{Contexts: []string{"review"}},
	})
	assert.Contains(t, s.FailingChecks, "review", "among VERDICTS the latest wins — the re-run failure is the verdict")
	assert.NotContains(t, s.SuccessfulChecks, "review")
	assert.False(t, ciAllGreen(s), "a PR with a failing verdict is not green")
}

func TestSnapshot_CancelledRequiredCheckIsFailure(t *testing.T) {
	// CANCELLED on a check that ran does not count as a pass. It is already in
	// failureConclusions, which failingChecks catches. This test proves the
	// end-to-end: a required check that concluded CANCELLED appears in
	// FailingChecks, not in SuccessfulChecks, and ciAllGreen is false.
	pr := mkPRWithCheckSuites(
		CheckSuite{
			Status: "COMPLETED", Conclusion: "CANCELLED", App: AppInfo{Name: "CI"},
		},
	)
	s := Snapshot(pr, SnapshotOptions{
		RulesetChecks: &RulesetChecks{Contexts: []string{"CI"}},
	})
	assert.Contains(t, s.FailingChecks, "CI")
	assert.NotContains(t, s.SuccessfulChecks, "CI")
	assert.Empty(t, s.AwaitingChecks, "CANCELLED check is present in payload, not awaiting")
	assert.False(t, ciAllGreen(s))
}

func TestSnapshot_SkippedNeutralRequiredSatisfied(t *testing.T) {
	// SKIPPED and NEUTRAL on a required check count as satisfied — the check
	// deliberately did not apply and no more events will arrive for it.
	for _, tc := range []struct{ name, conclusion string }{
		{"SKIPPED", "SKIPPED"},
		{"NEUTRAL", "NEUTRAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := mkPRWithCheckSuites(
				CheckSuite{Status: "COMPLETED", Conclusion: tc.conclusion, App: AppInfo{Name: "CI"}},
			)
			s := Snapshot(pr, SnapshotOptions{
				RulesetChecks: &RulesetChecks{Contexts: []string{"CI"}},
			})
			assert.Empty(t, s.FailingChecks)
			assert.Empty(t, s.PendingChecks)
			assert.Empty(t, s.AwaitingChecks)
			assert.Contains(t, s.SuccessfulChecks, "CI")
			assert.True(t, ciAllGreen(s))
		})
	}
}

func TestSnapshot_TruncatedSuites(t *testing.T) {
	// When the API reports more suites than were fetched (totalCount > len(nodes)),
	// TruncatedSuites must be set.
	t.Run("truncated suites detected", func(t *testing.T) {
		pr := &PullRequest{
			State: "OPEN",
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid:             "abc1234",
					MessageHeadline: "fix",
					CheckSuites:     SuiteNodes{TotalCount: 15, Nodes: []CheckSuite{}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.True(t, s.TruncatedSuites)
	})

	t.Run("no truncation when counts match", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"}},
		)
		s := Snapshot(pr, SnapshotOptions{})
		assert.False(t, s.TruncatedSuites)
	})

	t.Run("zero totalCount is not truncated", func(t *testing.T) {
		pr := &PullRequest{
			State: "OPEN",
			Commits: CommitNodes{Nodes: []Commit{{
				Commit: CommitDetails{
					Oid:             "abc1234",
					MessageHeadline: "fix",
					CheckSuites:     SuiteNodes{TotalCount: 0, Nodes: []CheckSuite{}},
				},
			}}},
		}
		s := Snapshot(pr, SnapshotOptions{})
		assert.False(t, s.TruncatedSuites)
	})

	t.Run("truncated check runs detected", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{
				Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"},
				CheckRuns: RunNodes{TotalCount: 20, Nodes: []CheckRun{
					{Name: "unit", Conclusion: "SUCCESS", Status: "COMPLETED"},
				}},
			},
		)
		s := Snapshot(pr, SnapshotOptions{})
		assert.True(t, s.TruncatedSuites, "truncated runs must set TruncatedSuites")
	})

	t.Run("untruncated runs when counts match", func(t *testing.T) {
		pr := mkPRWithCheckSuites(
			CheckSuite{
				Conclusion: "SUCCESS", Status: "COMPLETED", App: AppInfo{Name: "CI"},
				CheckRuns: RunNodes{TotalCount: 2, Nodes: []CheckRun{
					{Name: "unit", Conclusion: "SUCCESS", Status: "COMPLETED"},
					{Name: "lint", Conclusion: "SUCCESS", Status: "COMPLETED"},
				}},
			},
		)
		s := Snapshot(pr, SnapshotOptions{})
		assert.False(t, s.TruncatedSuites)
	})
}

func TestDiff_AwaitingChecksPreventsCIAllGreen(t *testing.T) {
	// When awaitingChecks is non-empty, curr is not clean, so the ci-all-green
	// transition must not fire even though failingChecks and pendingChecks are
	// empty.
	t.Run("awaiting checks prevent green transition", func(t *testing.T) {
		prev := &PRStatus{FailingChecks: []string{"CI"}}
		curr := &PRStatus{
			AwaitingChecks: []string{"security-scan"},
		}
		events := Diff(prev, curr)
		assert.Nil(t, findEvent(events, EventCIAllGreen),
			"ci-all-green must not fire when awaiting checks are present")
	})

	t.Run("ruleset error prevents green transition", func(t *testing.T) {
		prev := &PRStatus{FailingChecks: []string{"CI"}}
		curr := &PRStatus{
			RulesetError: "cannot read ruleset: 403",
		}
		events := Diff(prev, curr)
		assert.Nil(t, findEvent(events, EventCIAllGreen),
			"ci-all-green must not fire when ruleset error is present")
	})

	t.Run("clearing awaiting checks enables green transition", func(t *testing.T) {
		prev := &PRStatus{AwaitingChecks: []string{"security-scan"}}
		curr := &PRStatus{}
		events := Diff(prev, curr)
		assert.NotNil(t, findEvent(events, EventCIAllGreen),
			"ci-all-green must fire when awaiting checks clear")
	})
}

func TestRun_FirstPollWithAwaitingChecks(t *testing.T) {
	// ciAllGreen returns false when AwaitingChecks is non-empty, regardless of
	// the poll loop. This is a unit-level assertion on the ciAllGreen predicate.
	s := &PRStatus{
		State:            "OPEN",
		SuccessfulChecks: []string{"CI"},
		AwaitingChecks:   []string{"security-scan"},
	}
	assert.False(t, ciAllGreen(s), "ci-all-green must be false with awaiting checks")

	// When awaiting checks are cleared and everything else is green, ciAllGreen is true.
	s.AwaitingChecks = nil
	assert.True(t, ciAllGreen(s))
}

func TestFingerprint_StableForIdenticalSnapshot(t *testing.T) {
	a := mkPR("OPEN", false, "abc1234", []string{"ci-build"})
	b := mkPR("OPEN", false, "abc1234", []string{"ci-build"})
	assert.Equal(t, Fingerprint(a), Fingerprint(b))
	assert.NotEmpty(t, Fingerprint(a))
}

func TestFingerprint_ChangesOnRelevantFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PullRequest)
	}{
		{"new commit oid", func(p *PullRequest) { p.Commits.Nodes[0].Commit.Oid = "def5678" }},
		{"merge state", func(p *PullRequest) { p.Mergeable = "CONFLICTING" }},
		{"closed", func(p *PullRequest) { p.State = "CLOSED" }},
		{"check conclusion", func(p *PullRequest) {
			p.Commits.Nodes[0].Commit.CheckSuites.Nodes[0].CheckRuns.Nodes[0].Conclusion = "SUCCESS"
		}},
		{"review decision", func(p *PullRequest) {
			p.Reviews = ReviewNodes{Nodes: []Review{{State: "APPROVED", Author: struct {
				Login string `json:"login"`
			}{Login: "alice"}, SubmittedAt: "2026-01-01T00:00:00Z"}}}
		}},
		{"new general comment", func(p *PullRequest) {
			p.Comments = CommentNodes{Nodes: []Comment{{ID: "c1", Author: struct {
				Login string `json:"login"`
			}{Login: "alice"}}}}
		}},
		{"thread resolved", func(p *PullRequest) {
			p.ReviewThreads = ThreadNodes{Nodes: []ReviewThread{{ID: "t1", IsResolved: true}}}
		}},
		{"new annotation", func(p *PullRequest) {
			p.Commits.Nodes[0].Commit.CheckSuites.Nodes[0].CheckRuns.Nodes[0].Annotations = AnnotationNodes{
				Nodes: []Annotation{{Path: "a.go", Level: "WARNING", Title: "t", Message: "m"}},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := mkPR("OPEN", false, "abc1234", []string{"ci-build"})
			b := mkPR("OPEN", false, "abc1234", []string{"ci-build"})
			tc.mut(b)
			assert.NotEqual(t, Fingerprint(a), Fingerprint(b), "%s must change the fingerprint", tc.name)
		})
	}
}

func TestFingerprint_IgnoresCommentBodyEdit(t *testing.T) {
	// A comment body edit with the same ID produces no Diff event today
	// (diffNewComments matches by ID), so it must not count as a change here
	// either — otherwise a quiet PR would never idle-back off.
	a := mkPR("OPEN", false, "abc1234", []string{"ci-build"})
	a.Comments = CommentNodes{Nodes: []Comment{{ID: "c1", Body: "v1", Author: struct {
		Login string `json:"login"`
	}{Login: "alice"}}}}
	b := mkPR("OPEN", false, "abc1234", []string{"ci-build"})
	b.Comments = CommentNodes{Nodes: []Comment{{ID: "c1", Body: "v2", Author: struct {
		Login string `json:"login"`
	}{Login: "alice"}}}}
	assert.Equal(t, Fingerprint(a), Fingerprint(b))
}
