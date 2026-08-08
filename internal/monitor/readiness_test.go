package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Readiness query tests
// ---------------------------------------------------------------------------

func TestFetchReadiness(t *testing.T) {
	t.Run("sends query and unmarshals", func(t *testing.T) {
		api := &fakeAPI{graphqlFunc: func(query string, variables map[string]interface{}, result interface{}) error {
			assert.Contains(t, query, "MonitorReadiness")
			assert.Equal(t, "octocat", variables["owner"])
			assert.Equal(t, "hello", variables["repo"])
			assert.Equal(t, 100, variables["first"])
			return assign(result, ReadinessQueryResponse{
				Repository: struct {
					PullRequests ReadinessPRNodes `json:"pullRequests"`
				}{
					PullRequests: ReadinessPRNodes{
						TotalCount: 1,
						Nodes: []ReadinessPR{
							{
								Number:           1,
								State:            "OPEN",
								Mergeable:        "MERGEABLE",
								MergeStateStatus: "CLEAN",
								Author:           struct{ Login string `json:"login"` }{Login: "alice"},
							},
						},
					},
				},
			})
		}}
		svc := &Service{API: api}
		got, err := svc.FetchReadiness("octocat", "hello")
		require.NoError(t, err)
		require.Len(t, got.Repository.PullRequests.Nodes, 1)
		assert.Equal(t, 1, got.Repository.PullRequests.Nodes[0].Number)
		assert.Equal(t, "alice", got.Repository.PullRequests.Nodes[0].Author.Login)
	})
}

// ---------------------------------------------------------------------------
// Classification tests
// ---------------------------------------------------------------------------

func makeReadinessPR(number int, author string, mergeable string, mergeStateStatus string) ReadinessPR {
	return ReadinessPR{
		Number:           number,
		State:            "OPEN",
		Mergeable:        mergeable,
		MergeStateStatus: mergeStateStatus,
		Author:           struct{ Login string `json:"login"` }{Login: author},
	}
}

// makeReadinessPRWithCI creates a readiness PR with one successful check and no failures.
func makeReadinessPRWithCI(number int, author string, mergeable string, mergeStateStatus string) ReadinessPR {
	rp := makeReadinessPR(number, author, mergeable, mergeStateStatus)
	rp.Commits = CommitNodes{Nodes: []Commit{
		{Commit: CommitDetails{
			Oid:             "abc1234",
			MessageHeadline: "test",
			CheckSuites: SuiteNodes{Nodes: []CheckSuite{
				{
					Conclusion: "SUCCESS",
					Status:     "COMPLETED",
					App:        AppInfo{Name: "CI", Slug: "ci"},
					CheckRuns: RunNodes{Nodes: []CheckRun{
						{Name: "test", Conclusion: "SUCCESS", Status: "COMPLETED"},
					}},
				},
			}},
		}},
	}}
	return rp
}

// makeReadinessPRWithFailingCheck creates a readiness PR with one failing check.
func makeReadinessPRWithFailingCheck(number int, author string) ReadinessPR {
	rp := makeReadinessPR(number, author, "MERGEABLE", "CLEAN")
	rp.Commits = CommitNodes{Nodes: []Commit{
		{Commit: CommitDetails{
			Oid:             "abc1234",
			MessageHeadline: "test",
			CheckSuites: SuiteNodes{Nodes: []CheckSuite{
				{
					Conclusion: "FAILURE",
					Status:     "COMPLETED",
					App:        AppInfo{Name: "gofmt", Slug: "gofmt"},
					CheckRuns: RunNodes{Nodes: []CheckRun{
						{Name: "lint", Conclusion: "FAILURE", Status: "COMPLETED"},
					}},
				},
			}},
		}},
	}}
	return rp
}

// makeReadinessPRWithPending creates a readiness PR with a pending check and no successful ones.
func makeReadinessPRWithPending(number int, author string) ReadinessPR {
	rp := makeReadinessPR(number, author, "MERGEABLE", "CLEAN")
	rp.Commits = CommitNodes{Nodes: []Commit{
		{Commit: CommitDetails{
			Oid:             "abc1234",
			MessageHeadline: "test",
			CheckSuites: SuiteNodes{Nodes: []CheckSuite{
				{
					Status: "IN_PROGRESS",
					App:    AppInfo{Name: "CI", Slug: "ci"},
					CheckRuns: RunNodes{Nodes: []CheckRun{
						{Name: "build", Status: "IN_PROGRESS"},
					}},
				},
			}},
		}},
	}}
	return rp
}

// makeReadinessPRWithConflict creates a readiness PR with merge conflict.
func makeReadinessPRWithConflict(number int, author string) ReadinessPR {
	rp := makeReadinessPR(number, author, "CONFLICTING", "DIRTY")
	rp.Commits = CommitNodes{Nodes: []Commit{
		{Commit: CommitDetails{
			Oid:             "abc1234",
			MessageHeadline: "test",
			CheckSuites: SuiteNodes{Nodes: []CheckSuite{
				{
					Conclusion: "SUCCESS",
					Status:     "COMPLETED",
					App:        AppInfo{Name: "CI", Slug: "ci"},
					CheckRuns: RunNodes{Nodes: []CheckRun{
						{Name: "test", Conclusion: "SUCCESS", Status: "COMPLETED"},
					}},
				},
			}},
		}},
	}}
	return rp
}

// makeReadinessPRWithReviewChanges creates a PR with changes-requested review.
func makeReadinessPRWithReviewChanges(number int, author string) ReadinessPR {
	rp := makeReadinessPRWithCI(number, author, "MERGEABLE", "CLEAN")
	rp.Reviews = ReviewNodes{Nodes: []Review{
		{State: "CHANGES_REQUESTED", Author: struct{ Login string `json:"login"` }{Login: "reviewer"}},
	}}
	return rp
}

// makeReadinessPRBlocked creates a BLOCKED PR with clean CI.
func makeReadinessPRBlocked(number int, author string) ReadinessPR {
	rp := makeReadinessPRWithCI(number, author, "MERGEABLE", "BLOCKED")
	return rp
}

func TestClassifyPRsFull_CountsReconcile(t *testing.T) {
	t.Run("every PR lands in exactly one bucket", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithCI(1, "viewer", "MERGEABLE", "CLEAN"),
			makeReadinessPRWithFailingCheck(2, "viewer"),
			makeReadinessPRWithCI(3, "other", "MERGEABLE", "CLEAN"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Equal(t, 3, report.Open)

		err := report.Reconcile()
		assert.Empty(t, err, "reconciliation failed: %s", err)

		assert.Len(t, report.Ready, 1)
		assert.Equal(t, 1, report.Ready[0].Number)

		assert.Len(t, report.NotReady, 1)
		assert.Equal(t, 2, report.NotReady[0].Number)

		assert.Len(t, report.Others, 1)
		assert.Equal(t, 3, report.Others[0].Number)

		assert.Len(t, report.Unknown, 0)
	})
}

func TestClassifyPRsFull_AbsentRequiredCheck(t *testing.T) {
	t.Run("PR with absent required check is not ready", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithCI(1, "viewer", "MERGEABLE", "CLEAN"),
		}
		ruleset := &RulesetChecks{Contexts: []string{"missing-check"}}
		report := ClassifyPRsFull(prs, "viewer", ruleset)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "awaiting:missing-check")
	})
}

func TestClassifyPRsFull_BlockedByViewer(t *testing.T) {
	t.Run("BLOCKED authored by viewer → needs-codeowner", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRBlocked(1, "viewer"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "needs-codeowner")
		assert.NotContains(t, report.NotReady[0].Reason, "awaiting:review")
	})
}

func TestClassifyPRsFull_BlockedByOther(t *testing.T) {
	t.Run("BLOCKED authored by someone else → awaiting:review", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRBlocked(2, "other"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.Others, 1)
		assert.Contains(t, report.Others[0].Reason, "awaiting:review")
	})
}

func TestClassifyPRsFull_DegradedRuleset(t *testing.T) {
	t.Run("ruleset error → not ready with ruleset reason", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithCI(1, "viewer", "MERGEABLE", "CLEAN"),
		}
		ruleset := &RulesetChecks{Error: "403 Forbidden"}
		report := ClassifyPRsFull(prs, "viewer", ruleset)
		// Ruleset error should propagate to the snapshot and disqualify from ready.
		// However, Snapshot sets RulesetError but doesn't disqualify from ready
		// by itself — we handle that in readabilityReasonFull.
		// The PR should be not-ready with "ruleset:403 Forbidden".
		assert.Len(t, report.Ready, 0, "PR with unreadable ruleset must not be ready")
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "ruleset:")
	})
}

func TestClassifyPRsFull_Conflict(t *testing.T) {
	t.Run("conflicting PR → CONFLICTS", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithConflict(1, "viewer"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "CONFLICTS")
	})
}

func TestClassifyPRsFull_FailingCheck(t *testing.T) {
	t.Run("failing check → red:<check>", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithFailingCheck(1, "viewer"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "red:")
	})
}

func TestClassifyPRsFull_PendingCheck(t *testing.T) {
	t.Run("pending check → pending:<check>", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithPending(1, "viewer"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "pending:CI")
	})
}

func TestClassifyPRsFull_ReviewChangesRequested(t *testing.T) {
	t.Run("changes requested → not ready", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithReviewChanges(1, "viewer"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "changes-requested")
	})
}

func TestClassifyPRsFull_MergeabilityUnknown(t *testing.T) {
	t.Run("mergeability UNKNOWN → not ready", func(t *testing.T) {
		prs := []ReadinessPR{
			makeReadinessPRWithCI(1, "viewer", "UNKNOWN", "UNKNOWN"),
		}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "mergeability-unknown")
	})
}

func TestClassifyPRsFull_SkippedNeutralCountsAsSuccess(t *testing.T) {
	t.Run("SKIPPED/NEUTRAL checks settle the check", func(t *testing.T) {
		rp := makeReadinessPR(1, "viewer", "MERGEABLE", "CLEAN")
		rp.Commits = CommitNodes{Nodes: []Commit{
			{Commit: CommitDetails{
				Oid:             "abc1234",
				MessageHeadline: "test",
				CheckSuites: SuiteNodes{Nodes: []CheckSuite{
					{
						Conclusion: "NEUTRAL",
						Status:     "COMPLETED",
						App:        AppInfo{Name: "CI", Slug: "ci"},
						CheckRuns: RunNodes{Nodes: []CheckRun{
							{Name: "skipped-check", Conclusion: "SKIPPED", Status: "COMPLETED"},
						}},
					},
				}},
			}},
		}}
		prs := []ReadinessPR{rp}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 1, "SKIPPED/NEUTRAL checks should count as success (they settle)")
	})
}

func TestClassifyPRsFull_TruncatedSuites(t *testing.T) {
	t.Run("truncated suites → not ready", func(t *testing.T) {
		rp := makeReadinessPRWithCI(1, "viewer", "MERGEABLE", "CLEAN")
		// Add more suites than nodes to trigger truncation.
		rp.Commits.Nodes[0].Commit.CheckSuites.TotalCount = 100
		prs := []ReadinessPR{rp}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0, "truncated suites must not classify as ready")
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "truncated")
	})
}

func TestClassifyPRsFull_AllBucketsEmpty(t *testing.T) {
	t.Run("empty input → empty report", func(t *testing.T) {
		report := ClassifyPRsFull(nil, "viewer", nil)
		assert.Equal(t, 0, report.Open)
		assert.Empty(t, report.Ready)
		assert.Empty(t, report.NotReady)
		assert.Empty(t, report.Others)
		assert.Empty(t, report.Unknown)
		assert.Empty(t, report.Reconcile())
	})
}

// ---------------------------------------------------------------------------
// Report format tests
// ---------------------------------------------------------------------------

func TestReadinessReport_Format(t *testing.T) {
	report := &ReadinessReport{
		Owner:  "octocat",
		Repo:   "hello",
		Open:   4,
		Viewer: "viewer",
		Ready:  []PRReadiness{{Number: 1, Author: "viewer", Bucket: BucketReady}},
		NotReady: []PRReadiness{
			{Number: 2, Author: "viewer", Bucket: BucketNotReady, Reason: "needs-codeowner"},
		},
		Others: []PRReadiness{
			{Number: 3, Author: "other", Bucket: BucketOthers, Reason: "red:gofmt"},
		},
	}
	// Add a ready PR authored by someone else (goes to others with reason "ready").
	report.Others = append(report.Others, PRReadiness{Number: 4, Author: "bob", Bucket: BucketOthers, Reason: "ready"})
	report.Open = 4

	s := report.Sorted().Format()
	assert.Contains(t, s, "staging=success")
	assert.Contains(t, s, "open=4")
	assert.Contains(t, s, "ready=[1]")
	assert.Contains(t, s, "not-ready=[2(needs-codeowner)]")
	assert.Contains(t, s, "others=[3@other:red:gofmt 4@bob:ready]")
}

func TestReadinessReport_FormatDegraded(t *testing.T) {
	report := &ReadinessReport{
		Owner:           "octocat",
		Repo:            "hello",
		Degraded:        true,
		DegradedMessage: "graphql error: connection refused",
	}
	s := report.Format()
	assert.Contains(t, s, "staging=degraded")
	assert.Contains(t, s, "graphql error")
}

func TestReadinessReport_Reconcile(t *testing.T) {
	t.Run("clean report passes", func(t *testing.T) {
		report := &ReadinessReport{
			Open:  3,
			Ready: []PRReadiness{{Number: 1}},
			NotReady: []PRReadiness{{Number: 2}},
			Others: []PRReadiness{{Number: 3}},
		}
		assert.Empty(t, report.Reconcile())
	})

	t.Run("count mismatch fails", func(t *testing.T) {
		report := &ReadinessReport{
			Open:  3,
			Ready: []PRReadiness{{Number: 1}},
		}
		err := report.Reconcile()
		assert.Contains(t, err, "count mismatch")
	})

	t.Run("duplicate across buckets fails", func(t *testing.T) {
		report := &ReadinessReport{
			Open:  2,
			Ready: []PRReadiness{{Number: 1}},
			NotReady: []PRReadiness{{Number: 1}},
		}
		err := report.Reconcile()
		assert.Contains(t, err, "duplicate")
	})
}

// ---------------------------------------------------------------------------
// Conflict + failing check dedup test
// ---------------------------------------------------------------------------

func TestClassifyPRsFull_ConflictDeduplicatesRed(t *testing.T) {
	t.Run("conflict + failing checks → only CONFLICTS, no red:", func(t *testing.T) {
		rp := makeReadinessPR(1, "viewer", "CONFLICTING", "DIRTY")
		rp.Commits = CommitNodes{Nodes: []Commit{
			{Commit: CommitDetails{
				Oid:             "abc1234",
				MessageHeadline: "test",
				CheckSuites: SuiteNodes{Nodes: []CheckSuite{
					{
						Conclusion: "FAILURE",
						Status:     "COMPLETED",
						App:        AppInfo{Name: "gofmt", Slug: "gofmt"},
						CheckRuns: RunNodes{Nodes: []CheckRun{
							{Name: "lint", Conclusion: "FAILURE", Status: "COMPLETED"},
						}},
					},
				}},
			}},
		}}
		prs := []ReadinessPR{rp}
		report := ClassifyPRsFull(prs, "viewer", nil)
		assert.Len(t, report.Ready, 0)
		assert.Len(t, report.NotReady, 1)
		assert.Contains(t, report.NotReady[0].Reason, "CONFLICTS")
		assert.NotContains(t, report.NotReady[0].Reason, "red:")
	})
}
