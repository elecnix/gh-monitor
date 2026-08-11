package monitor

import "fmt"

// QueryTier controls how much of the monitoring snapshot a poll fetches. The
// tiers follow the operator's priority order — PR status → GitHub Actions
// outcomes → comments → reviews → annotations — so a watcher under a tight
// GraphQL budget sheds the least valuable surfaces first and keeps the most
// valuable ones (PR state, mergeability, check outcomes) alive in every tier.
//
// A lower tier fetches fewer fields, so each poll costs fewer GraphQL points
// and the watcher's share of the hourly budget lasts longer. Shedding is
// advisory and loud: the tier is selected from the rate limit before each
// poll, and a transition into or out of a shed tier emits a notification
// naming exactly what is no longer being watched (see CarryForwardShed for
// how shed surfaces keep their last-known values instead of reading as
// "cleared").
type QueryTier int

const (
	// TierStatus fetches only PR state + check outcomes. Comments, review
	// threads, reviews, and annotations are shed.
	TierStatus QueryTier = iota
	// TierNoReviews also sheds reviews and review threads (annotations too).
	TierNoReviews
	// TierNoAnnotations sheds only annotations — the dominant per-query cost
	// (the #52 fix cut their nested limits for exactly this reason).
	TierNoAnnotations
	// TierFull fetches everything: the pre-degradation query.
	TierFull
)

// HasComments reports whether the tier still fetches general comments.
func (t QueryTier) HasComments() bool { return t >= TierNoReviews }

// HasReviews reports whether the tier still fetches reviews and review threads.
func (t QueryTier) HasReviews() bool { return t >= TierNoAnnotations }

// HasAnnotations reports whether the tier still fetches check-run annotations.
func (t QueryTier) HasAnnotations() bool { return t >= TierFull }

// String returns a stable name for the tier, used in test names and logs.
func (t QueryTier) String() string {
	switch t {
	case TierFull:
		return "TierFull"
	case TierNoAnnotations:
		return "TierNoAnnotations"
	case TierNoReviews:
		return "TierNoReviews"
	case TierStatus:
		return "TierStatus"
	}
	return fmt.Sprintf("QueryTier(%d)", int(t))
}

// ShedSurfaces names the surfaces the tier does NOT fetch, in the operator's
// priority order (least valuable first). The empty slice means nothing is
// shed. The names are stable identifiers used in notifications and JSON.
func (t QueryTier) ShedSurfaces() []string {
	switch t {
	case TierFull:
		return nil
	case TierNoAnnotations:
		return []string{"annotations"}
	case TierNoReviews:
		return []string{"annotations", "reviews", "review threads"}
	case TierStatus:
		return []string{"annotations", "reviews", "review threads", "comments"}
	}
	return nil
}

// Lower returns the next tier down (shedding one more surface), clamping at
// TierStatus. It is the fallback target when a query exceeds the per-query
// resource limit: the cheaper query may pass where the richer one failed.
func (t QueryTier) Lower() QueryTier {
	if t <= TierStatus {
		return TierStatus
	}
	return t - 1
}

// TierForRemaining maps an advisory GraphQL budget to a tier. The thresholds
// are deliberately conservative: the watcher starts shedding well before
// exhaustion so its PR-status + checks coverage survives longest.
func TierForRemaining(remaining, limit int) QueryTier {
	if limit <= 0 {
		return TierFull
	}
	pct := float64(remaining) / float64(limit)
	switch {
	case pct >= 0.20:
		return TierFull
	case pct >= 0.08:
		return TierNoAnnotations
	case pct >= 0.02:
		return TierNoReviews
	default:
		return TierStatus
	}
}

// ---------------------------------------------------------------------------
// Tiered query builders
// ---------------------------------------------------------------------------

// monitorPRTemplate has two %s slots: the middle surfaces (comments, review
// threads, reviews) and the annotations block inside checkRuns. The query has
// no '%' characters of its own, so Sprintf is safe.
const monitorPRTemplate = `query MonitorPR($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      state
      merged
      mergeable
      mergeStateStatus
%s      commits(last: 1) {
        nodes {
          commit {
            oid
            messageHeadline
            message
            authors(first: 10) { nodes { name user { login } } }
            checkSuites(last: 50) {
              totalCount
              nodes {
                conclusion
                status
                app { name slug }
                checkRuns(last: 50) {
                  nodes { name conclusion status detailsUrl permalink
%s
                  }
                }
              }
            }
            status { contexts { state context description targetUrl } }
          }
        }
      }
    }
  }
}`

const prCommentsFragment = `      comments(last: 25) {
        nodes {
          id
          body
          author { login }
          createdAt
          reactionGroups { content users { totalCount } }
        }
      }
`
const prThreadsFragment = `      reviewThreads(last: 25) {
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(last: 25) {
            nodes {
              id
              body
              author { login }
              createdAt
              diffHunk
              reactionGroups { content users { totalCount } }
            }
          }
        }
      }
`
const prReviewsFragment = `      reviews(last: 100) {
        nodes { state author { login } submittedAt }
      }
`
const annotationsFragment = `                    annotations(first: 50) {
                      totalCount
                      nodes { path location { start { line } } annotationLevel title message }
                    }
`

// MonitorQuery returns the PR snapshot query for the given tier.
func MonitorQuery(tier QueryTier) string {
	var middle string
	if tier.HasComments() {
		middle += prCommentsFragment
	}
	if tier.HasReviews() {
		middle += prThreadsFragment
		middle += prReviewsFragment
	}
	anns := ""
	if tier.HasAnnotations() {
		anns = annotationsFragment
	}
	return fmt.Sprintf(monitorPRTemplate, middle, anns)
}

// monitorRefCommitTemplate is shared by the ref and commit queries, whose only
// shed-able surface is annotations (they carry no comments or reviews). The
// first %s is the operation name (MonitorRef / MonitorCommit), the second the
// target fragment.
const monitorRefCommitTemplate = `query %s($owner: String!, $repo: String!, $oid: GitObjectID!, $ref: String!) {
  repository(owner: $owner, name: $repo) {
%s  }
}`

const refCommitTargetFragment = `    ref(qualifiedName: $ref) {
      target {
        oid
        ... on Commit {
          messageHeadline
          authors(first: 10) { nodes { name user { login } } }
          checkSuites(last: 50) {
            totalCount
            nodes {
              conclusion
              status
              app { name slug }
              checkRuns(last: 50) {
                nodes { name conclusion status detailsUrl permalink
%s
                }
              }
            }
          }
          status { contexts { state context description targetUrl } }
        }
      }
    }
`
const commitObjectFragment = `    object(oid: $oid) {
      ... on Commit {
        oid
        messageHeadline
        authors(first: 10) { nodes { name user { login } } }
        checkSuites(last: 50) {
          totalCount
          nodes {
            conclusion
            status
            app { name slug }
            checkRuns(last: 50) {
              nodes { name conclusion status detailsUrl permalink
%s
              }
            }
          }
        }
        status { contexts { state context description targetUrl } }
      }
    }
`

// MonitorRefQuery returns the ref snapshot query for the given tier.
func MonitorRefQuery(tier QueryTier) string {
	anns := ""
	if tier.HasAnnotations() {
		anns = annotationsFragment
	}
	return fmt.Sprintf(monitorRefCommitTemplate, "MonitorRef", fmt.Sprintf(refCommitTargetFragment, anns))
}

// MonitorCommitQuery returns the commit snapshot query for the given tier.
func MonitorCommitQuery(tier QueryTier) string {
	anns := ""
	if tier.HasAnnotations() {
		anns = annotationsFragment
	}
	return fmt.Sprintf(monitorRefCommitTemplate, "MonitorCommit", fmt.Sprintf(commitObjectFragment, anns))
}
