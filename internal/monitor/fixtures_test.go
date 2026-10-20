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

// mkAnnotation builds one check-run annotation.
func mkAnnotation(path, level, title, message string, line int) Annotation {
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

// mkAnnotatedPR builds a PR whose single finished suite carries the named
// check runs, each with one warning-level annotation.
//
// It is the fixture the check-name filter is about: one check a consumer
// watches, and one that posts the same warning from every workflow job.
func mkAnnotatedPR(runs ...CheckRun) *PullRequest {
	suite := CheckSuite{
		Status:     "COMPLETED",
		Conclusion: "SUCCESS",
		App:        AppInfo{Name: "CI"},
		CheckRuns:  RunNodes{Nodes: runs},
	}
	return &PullRequest{
		State: "OPEN",
		Commits: CommitNodes{Nodes: []Commit{{Commit: CommitDetails{
			Oid:         "abc",
			CheckSuites: SuiteNodes{Nodes: []CheckSuite{suite}},
		}}}},
	}
}

// mkAnnotatedRun builds a completed check run carrying one annotation.
func mkAnnotatedRun(name, path, title, message string, line int) CheckRun {
	return CheckRun{
		Name:       name,
		Status:     "COMPLETED",
		Conclusion: "SUCCESS",
		Annotations: AnnotationNodes{TotalCount: 1, Nodes: []Annotation{
			mkAnnotation(path, "WARNING", title, message, line),
		}},
	}
}
