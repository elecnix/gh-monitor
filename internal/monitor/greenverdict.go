package monitor

// The green verdict (issue #239).
//
// "Nothing failing or pending among the checks this read could see" is not
// the same sentence as "every check passed". A rerun removes the old check run
// before GitHub creates the new one, and a push moves the head to a commit
// whose check suites do not exist yet: in both windows the rollup is missing
// a check rather than reporting it, and an absent check is neither failing
// nor pending. This file holds the rule that tells the two sentences apart,
// shared by the pull-request and ref/commit diffs.

// Reasons a read with nothing failing or pending is still not green. The
// reason is stored on the snapshot, so the next read knows a green was owed
// even though this one looked clean.
const (
	// GreenWithheldMissing: a check that an earlier read of the same head
	// commit listed is absent from this read.
	GreenWithheldMissing = "missing"
	// GreenWithheldEmpty: the rollup names no passed check at all.
	GreenWithheldEmpty = "empty"
	// GreenWithheldNewHead: this read is the first one of a new head commit.
	// A verdict in the same breath as a push is the case that fooled #239.
	GreenWithheldNewHead = "new-head"
)

// ciView is the part of one CI read that the green verdict depends on.
type ciView struct {
	head       string
	failing    []string
	pending    []string
	successful []string
	awaiting   []string
	// observed names every check run in the payload, whatever its state, so
	// a run that is in progress counts as present.
	observed []string
	// blocked is set when the read cannot vouch for completeness: the
	// ruleset is unreadable, or the suite list was truncated.
	blocked bool
	// missing and withheld are what the previous judgement recorded.
	missing  []string
	withheld string
}

// quiet reports whether nothing in the read is failing, pending or
// unverifiable. Quiet is necessary for green, never sufficient.
func (v *ciView) quiet() bool {
	return len(v.failing) == 0 && len(v.pending) == 0 && len(v.awaiting) == 0 && !v.blocked
}

// greenJudgement is the outcome of judging one read against the previous one.
type greenJudgement struct {
	// missing names the checks an earlier read of this head listed and this
	// read does not. They carry forward until they reappear.
	missing []string
	// withheld is one of the GreenWithheld* reasons when the read is quiet
	// but not green, and empty otherwise.
	withheld string
	// green is true when CI just turned green: the previous read had work in
	// flight, or owed a green it withheld, and this read is green.
	green bool
}

// judgeGreen decides whether curr is a green CI transition from prev.
//
// Pending checks are reported by suite while finished ones are reported by
// run, so a vanished check is looked for among the names a read lists as
// failing, passed or as any check run, never among the pending suite names:
// a suite that finished is replaced by its runs' names, not lost.
func judgeGreen(prev, curr *ciView) greenJudgement {
	var j greenJudgement
	sameHead := prev != nil && prev.head == curr.head
	if sameHead {
		present := map[string]bool{}
		for _, list := range [][]string{curr.failing, curr.pending, curr.successful, curr.awaiting, curr.observed} {
			for _, name := range list {
				present[name] = true
			}
		}
		seen := map[string]bool{}
		for _, list := range [][]string{prev.failing, prev.successful, prev.observed, prev.missing} {
			for _, name := range list {
				if !present[name] && !seen[name] {
					seen[name] = true
					j.missing = append(j.missing, name)
				}
			}
		}
	}
	if !curr.quiet() {
		return j
	}
	switch {
	case len(j.missing) > 0:
		j.withheld = GreenWithheldMissing
	case len(curr.successful) == 0:
		j.withheld = GreenWithheldEmpty
	case prev != nil && prev.head != "" && !sameHead:
		j.withheld = GreenWithheldNewHead
	}
	prevOwed := prev != nil && (!prev.quiet() || prev.withheld != "")
	j.green = j.withheld == "" && prevOwed
	return j
}

func prCIView(s *PRStatus) *ciView {
	if s == nil {
		return nil
	}
	return &ciView{
		head:       s.LastCommit.Oid,
		failing:    s.FailingChecks,
		pending:    s.PendingChecks,
		successful: s.SuccessfulChecks,
		awaiting:   s.AwaitingChecks,
		observed:   s.ObservedChecks,
		blocked:    s.RulesetError != "" || s.TruncatedSuites,
		missing:    s.MissingChecks,
		withheld:   s.GreenWithheld,
	}
}

func refCIView(s *RefStatus) *ciView {
	if s == nil {
		return nil
	}
	return &ciView{
		head:       s.Oid,
		failing:    s.FailingChecks,
		pending:    s.PendingChecks,
		successful: s.SuccessfulChecks,
		observed:   s.ObservedChecks,
		missing:    s.MissingChecks,
		withheld:   s.GreenWithheld,
	}
}

// greenEvent builds the ci-all-green event with its evidence: the checks the
// read saw pass, by name, and the head commit the verdict belongs to.
func greenEvent(passed []string, head CommitSummary) Event {
	commit := head
	return Event{
		Type:         EventCIAllGreen,
		PassedChecks: append([]string(nil), passed...),
		Commit:       &commit,
	}
}
