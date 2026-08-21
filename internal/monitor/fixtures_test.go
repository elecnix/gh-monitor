package monitor

// Shared PR fixtures for the snapshot/diff/render tests. Mirroring terminal
// states matters — a suite with a blank status/conclusion is a state GitHub
// never reports, and it would look "clean" for the wrong reason (see
// AGENTS.md).

// mkCommit builds a commit whose CI has finished: the suite is COMPLETED, and
// it concludes SUCCESS unless failing names are supplied.
func mkCommit(oid string, failing []string) Commit {
	runs := make([]CheckRun, 0, len(failing))
	for _, name := range failing {
		runs = append(runs, CheckRun{Name: name, Conclusion: "FAILURE"})
	}
	suite := CheckSuite{Status: "COMPLETED", Conclusion: "SUCCESS", App: AppInfo{Name: "CI"}, CheckRuns: RunNodes{Nodes: runs}}
	if len(failing) > 0 {
		// The failing runs carry the failure; leave the suite conclusion blank
		// so the suite name doesn't also land in FailingChecks.
		suite.Conclusion = ""
	}
	return Commit{Commit: CommitDetails{
		Oid:             oid,
		MessageHeadline: "headline",
		CheckSuites:     SuiteNodes{Nodes: []CheckSuite{suite}},
	}}
}

func mkPR(state string, merged bool, oid string, failing []string) *PullRequest {
	return &PullRequest{
		State:   state,
		Merged:  merged,
		Commits: CommitNodes{Nodes: []Commit{mkCommit(oid, failing)}},
	}
}
