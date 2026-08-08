package monitor

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Readiness query — fetches open PRs with head-commit checkSuites in the same
// shape as MONITOR_QUERY so the existing Snapshot, awaitingChecks, ciAllGreen,
// and truncatedSuites functions work unchanged.
// ---------------------------------------------------------------------------

// MONITOR_READINESS_QUERY fetches every open PR with its author, review state,
// and the head commit's check suites (the same shape as MONITOR_QUERY).
// Comments, threads, and reactions are omitted to keep the payload lean.
const MONITOR_READINESS_QUERY = `query MonitorReadiness($owner: String!, $repo: String!, $first: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequests(first: $first, states: OPEN, orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      nodes {
        number
        state
        isDraft
        mergeable
        mergeStateStatus
        author { login }
        reviews(last: 100) {
          nodes { state author { login } submittedAt }
        }
        commits(last: 1) {
          nodes {
            commit {
              oid
              messageHeadline
              message
              authors(first: 1) { nodes { name user { login } } }
              checkSuites(last: 50) {
                totalCount
                nodes {
                  conclusion
                  status
                  app { name slug }
                  checkRuns(last: 50) {
                    nodes { name conclusion status }
                  }
                }
              }
              status { contexts { state context } }
            }
          }
        }
      }
    }
  }
}`

// ---------------------------------------------------------------------------
// Readiness response types
// ---------------------------------------------------------------------------

// ReadinessQueryResponse mirrors the GraphQL envelope for the readiness query.
type ReadinessQueryResponse struct {
	Repository struct {
		PullRequests ReadinessPRNodes `json:"pullRequests"`
	} `json:"repository"`
}

// ReadinessPRNodes holds the list of open PRs from the readiness query.
type ReadinessPRNodes struct {
	Nodes      []ReadinessPR `json:"nodes"`
	TotalCount int           `json:"totalCount"`
}

// ReadinessPR is a single open PR from the readiness query. It carries the
// same checkSuite shape as PullRequest so it can be converted for use with
// the existing Snapshot / awaitingChecks / ciAllGreen functions.
type ReadinessPR struct {
	Number           int    `json:"number"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	Author           struct {
		Login string `json:"login"`
	} `json:"author"`
	Reviews ReviewNodes `json:"reviews"`
	Commits CommitNodes `json:"commits"`
}

// ToPullRequest converts a readiness PR node into a *PullRequest with enough
// fields populated for the existing Snapshot, awaitingChecks, ciAllGreen,
// reviewDecision, and truncatedSuites to work. Comments/threads are left nil.
func (rp *ReadinessPR) ToPullRequest() *PullRequest {
	return &PullRequest{
		State:      rp.State,
		Mergeable:  rp.Mergeable,
		MergeState: rp.MergeStateStatus,
		Reviews:    rp.Reviews,
		Commits:    rp.Commits,
	}
}

// ---------------------------------------------------------------------------
// Readiness classification
// ---------------------------------------------------------------------------

// BucketClass is the readiness bucket for one PR.
type BucketClass string

const (
	BucketReady    BucketClass = "ready"
	BucketNotReady BucketClass = "not-ready"
	BucketOthers   BucketClass = "others"
	BucketUnknown  BucketClass = "unknown"
)

// PRReadiness is one PR's readiness classification.
type PRReadiness struct {
	Number int
	Author string
	Bucket BucketClass
	Reason string // empty for ready; reason label for not-ready/others/unknown
}

// ReadinessReport is a repo-wide merge-readiness snapshot.
type ReadinessReport struct {
	Owner   string
	Repo    string
	Open    int
	Viewer  string
	Ready   []PRReadiness
	NotReady []PRReadiness
	Others  []PRReadiness
	Unknown []PRReadiness

	// Degraded is set when the fetch failed — no classification performed.
	Degraded        bool
	DegradedMessage string
}

// ClassifyPRsFull classifies every open PR into a readiness bucket with full
// access to ReadinessPR fields (mergeStateStatus, mergeable).
//
// Bucketing rules:
//   - ready: authored by viewer, mergeable, no conflict, no failing/pending/
//     awaiting checks, ruleset readable, suites not truncated, at least one
//     successful check.
//   - not-ready (with reason): authored by viewer, not meeting ready criteria.
//   - others (with reason): authored by someone else, regardless of status.
//     Non-viewer PRs that would be "ready" appear as others with reason "ready".
//   - No PR is ever unclassified — if a PR somehow falls through every branch
//     it is surfaced in unknown with a diagnostic reason.
func ClassifyPRsFull(prs []ReadinessPR, viewer string, ruleset *RulesetChecks) *ReadinessReport {
	report := &ReadinessReport{
		Open:     len(prs),
		Viewer:   viewer,
		Ready:    make([]PRReadiness, 0),
		NotReady: make([]PRReadiness, 0),
		Others:   make([]PRReadiness, 0),
		Unknown:  make([]PRReadiness, 0),
	}

	snapOpts := SnapshotOptions{}
	if ruleset != nil {
		snapOpts.RulesetChecks = ruleset
	}

	for i := range prs {
		rp := &prs[i]
		pr := rp.ToPullRequest()
		author := rp.Author.Login

		status := Snapshot(pr, snapOpts)

		reason := readinessReasonFull(rp, status, viewer)

		if reason == "" {
			entry := PRReadiness{
				Number: rp.Number,
				Author: author,
			}
			if author == viewer {
				entry.Bucket = BucketReady
				report.Ready = append(report.Ready, entry)
			} else {
				entry.Bucket = BucketOthers
				entry.Reason = "ready"
				report.Others = append(report.Others, entry)
			}
			continue
		}

		// Not ready — determine bucket by authorship.
		entry := PRReadiness{
			Number: rp.Number,
			Author: author,
			Reason: reason,
		}

		if author == viewer {
			entry.Bucket = BucketNotReady
			report.NotReady = append(report.NotReady, entry)
		} else {
			entry.Bucket = BucketOthers
			report.Others = append(report.Others, entry)
		}
	}

	return report
}

// readinessReasonFull computes the readiness reason with full access to
// ReadinessPR fields (mergeStateStatus, mergeable). Returns "" when the PR
// is ready (all conditions met). Readiness is an explicit conjunction — never
// the leftover.
func readinessReasonFull(rp *ReadinessPR, status *PRStatus, viewer string) string {
	var reasons []string

	// A draft PR cannot be merged — disqualify before anything else.
	if rp.IsDraft {
		reasons = append(reasons, "draft")
	}

	// Degraded snapshot data — never classify as ready.
	if status.TruncatedSuites {
		reasons = append(reasons, "truncated")
	}
	if status.RulesetError != "" {
		reasons = append(reasons, "ruleset:"+status.RulesetError)
	}

	// Merge conflict.
	if status.Conflict {
		reasons = append(reasons, "CONFLICTS")
	}

	// Failing checks.
	for _, c := range status.FailingChecks {
		reasons = append(reasons, "red:"+c)
	}

	// Pending (in-flight) checks.
	for _, c := range status.PendingChecks {
		reasons = append(reasons, "pending:"+c)
	}

	// Awaiting (absent required) checks.
	for _, c := range status.AwaitingChecks {
		reasons = append(reasons, "awaiting:"+c)
	}

	// Review changes requested.
	if status.ReviewDecision == "CHANGES_REQUESTED" {
		reasons = append(reasons, "changes-requested")
	}

	// Merge state status — only a subset mean "ready to merge".
	// BLOCKED is handled below with the viewer distinction; every other
	// non-CLEAN value must produce a reason so nothing silently falls to ready.
	author := rp.Author.Login
	switch rp.MergeStateStatus {
	case "BLOCKED":
		if author == viewer {
			reasons = append(reasons, "needs-codeowner")
		} else {
			reasons = append(reasons, "awaiting:review")
		}
	case "BEHIND":
		reasons = append(reasons, "needs-rebase")
	case "DIRTY":
		// DIRTY means the merge commit failed — distinct from CONFLICTING,
		// which means a live conflict exists. Both block the merge.
		if !status.Conflict {
			reasons = append(reasons, "dirty-merge")
		}
	case "UNSTABLE":
		// UNSTABLE means a non-required check is failing. It is technically
		// still mergeable, but calling it ready would be misleading: the
		// viewer likely wants to know a check is red before they merge.
		reasons = append(reasons, "unstable")
	case "UNKNOWN":
		// GitHub is still computing mergeability — not the same as clean.
		reasons = append(reasons, "mergeability-unknown")
	case "CLEAN":
		// Nothing wrong — proceed to check-derived reasons.
	default:
		// An unrecognised mergeStateStatus must not silently produce ready.
		reasons = append(reasons, "merge-state:"+rp.MergeStateStatus)
	}

	// No CI has reported yet (no successful checks, no failing checks, no
	// pending checks). This is distinct from all-green.
	if len(status.SuccessfulChecks) == 0 && len(status.FailingChecks) == 0 &&
		len(status.PendingChecks) == 0 && len(status.AwaitingChecks) == 0 &&
		status.RulesetError == "" && !status.TruncatedSuites {
		reasons = append(reasons, "no-ci")
	}

	// If we have any reasons, the PR is not ready.
	if len(reasons) > 0 {
		// Deduplicate "red:" and "CONFLICTS" — conflict can cause CI failures,
		// and reporting both is noisy. Keep CONFLICTS and drop red: when both
		// are present.
		hasConflict := false
		for _, r := range reasons {
			if r == "CONFLICTS" {
				hasConflict = true
				break
			}
		}
		if hasConflict {
			filtered := make([]string, 0, len(reasons))
			for _, r := range reasons {
				if strings.HasPrefix(r, "red:") {
					continue // conflict explains the failures
				}
				filtered = append(filtered, r)
			}
			reasons = filtered
		}

		return strings.Join(reasons, ",")
	}

	// All conditions passed: CI green, no conflict, ruleset readable, suites
	// not truncated, at least one successful check → ready.
	return ""
}

// ---------------------------------------------------------------------------
// Readiness service
// ---------------------------------------------------------------------------

// FetchReadiness fetches all open PRs for a repo using the readiness query.
func (s *Service) FetchReadiness(owner, repo string) (*ReadinessQueryResponse, error) {
	var result ReadinessQueryResponse
	// Fetch up to 100 open PRs (GitHub GraphQL default page size).
	err := s.API.GraphQL(MONITOR_READINESS_QUERY, map[string]interface{}{
		"owner": owner,
		"repo":  repo,
		"first": 100,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Report formatting
// ---------------------------------------------------------------------------

// Format returns the one-line readiness report string.
//
//	format: staging=<status> open=<N> ready=[<prs>] not-ready=[<prs>] others=[<prs>] [unknown=[<prs>]]
//
// Each not-ready entry: <number>(<reason>)
// Each others entry: <number>@<author>:<reason>
// Ready entries: just the numbers
func (r *ReadinessReport) Format() string {
	var b strings.Builder

	// Staging status.
	if r.Degraded {
		b.WriteString("staging=degraded")
		if r.DegradedMessage != "" {
			b.WriteString("(")
			b.WriteString(truncateDegradedMsg(r.DegradedMessage, 60))
			b.WriteString(")")
		}
		// Degraded: counts are unavailable — never render them as numbers.
		b.WriteString(" open=? ready=? not-ready=? others=?")
		if len(r.Unknown) > 0 {
			b.WriteString(" unknown=?")
		}
		return b.String()
	}

	b.WriteString("staging=success")

	fmt.Fprintf(&b, " open=%d", r.Open)

	// Ready.
	b.WriteString(" ready=[")
	formatNumbers(&b, r.Ready, func(pr PRReadiness) string {
		return fmt.Sprintf("%d", pr.Number)
	})
	b.WriteString("]")

	// Not-ready (viewer's own PRs).
	b.WriteString(" not-ready=[")
	formatNumbers(&b, r.NotReady, func(pr PRReadiness) string {
		return fmt.Sprintf("%d(%s)", pr.Number, pr.Reason)
	})
	b.WriteString("]")

	// Others (someone else's PRs).
	b.WriteString(" others=[")
	formatNumbers(&b, r.Others, func(pr PRReadiness) string {
		return fmt.Sprintf("%d@%s:%s", pr.Number, pr.Author, pr.Reason)
	})
	b.WriteString("]")

	// Unknown (should always be empty).
	if len(r.Unknown) > 0 {
		b.WriteString(" unknown=[")
		formatNumbers(&b, r.Unknown, func(pr PRReadiness) string {
			s := fmt.Sprintf("%d", pr.Number)
			if pr.Reason != "" {
				s += "(" + pr.Reason + ")"
			}
			return s
		})
		b.WriteString("]")
	}

	return b.String()
}

func formatNumbers(b *strings.Builder, prs []PRReadiness, format func(PRReadiness) string) {
	for i, pr := range prs {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(format(pr))
	}
}

func truncateDegradedMsg(msg string, maxLen int) string {
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
}

// Reconcile verifies that every open PR landed in exactly one bucket and the
// counts sum to the open total. Returns "" on success or an error message.
func (r *ReadinessReport) Reconcile() string {
	total := len(r.Ready) + len(r.NotReady) + len(r.Others) + len(r.Unknown)
	if total != r.Open {
		return fmt.Sprintf("count mismatch: open=%d but buckets sum to %d (ready=%d not-ready=%d others=%d unknown=%d)",
			r.Open, total, len(r.Ready), len(r.NotReady), len(r.Others), len(r.Unknown))
	}
	// Check for duplicates across buckets.
	seen := map[int]string{}
	for _, pr := range r.Ready {
		seen[pr.Number] = "ready"
	}
	for _, pr := range r.NotReady {
		if prev, ok := seen[pr.Number]; ok {
			return fmt.Sprintf("duplicate PR #%d in ready and not-ready (was %s)", pr.Number, prev)
		}
		seen[pr.Number] = "not-ready"
	}
	for _, pr := range r.Others {
		if prev, ok := seen[pr.Number]; ok {
			return fmt.Sprintf("duplicate PR #%d in %s and others", pr.Number, prev)
		}
		seen[pr.Number] = "others"
	}
	for _, pr := range r.Unknown {
		if prev, ok := seen[pr.Number]; ok {
			return fmt.Sprintf("duplicate PR #%d in %s and unknown", pr.Number, prev)
		}
		seen[pr.Number] = "unknown"
	}
	return ""
}

// Sorted returns a copy of the report with all buckets sorted by PR number
// ascending for stable output.
func (r *ReadinessReport) Sorted() *ReadinessReport {
	sortByNumber := func(prs []PRReadiness) {
		sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	}
	sortByNumber(r.Ready)
	sortByNumber(r.NotReady)
	sortByNumber(r.Others)
	sortByNumber(r.Unknown)
	return r
}
