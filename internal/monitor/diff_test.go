package monitor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventTypes extracts the ordered Type list from events.
func eventTypes(events []Event) []EventType {
	out := make([]EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// findEvent returns the first event of the given type, or nil.
func findEvent(events []Event, t EventType) *Event {
	for i := range events {
		if events[i].Type == t {
			return &events[i]
		}
	}
	return nil
}

func TestDiff_FirstPollBaselineSilent(t *testing.T) {
	// prev == nil establishes a baseline and must emit nothing, even when the
	// current snapshot is full of pre-existing state.
	curr := &PRStatus{
		State:             "OPEN",
		Conflict:          true,
		FailingChecks:     []string{"CI"},
		PendingChecks:     []string{"Deploy"},
		UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1"}}},
		GeneralComments:   []GeneralComment{{ID: "G1", Author: "alice", Body: "hi"}},
		ReviewDecision:    "CHANGES_REQUESTED",
		ReviewAuthor:      "bob",
		LastCommit:        CommitSummary{Oid: "abc"},
	}
	assert.Empty(t, Diff(nil, curr))
}

func TestDiff_NewFailingChecks(t *testing.T) {
	prev := &PRStatus{FailingChecks: []string{"CI"}}
	curr := &PRStatus{FailingChecks: []string{"CI", "lint"}}
	events := Diff(prev, curr)
	e := findEvent(events, EventNewFailingChecks)
	require.NotNil(t, e)
	assert.Equal(t, []string{"lint"}, e.Checks)
}

func TestDiff_NoEventWhenSameFailing(t *testing.T) {
	prev := &PRStatus{FailingChecks: []string{"CI"}}
	curr := &PRStatus{FailingChecks: []string{"CI"}}
	assert.Nil(t, findEvent(Diff(prev, curr), EventNewFailingChecks))
}

func TestDiff_CIAllGreen(t *testing.T) {
	t.Run("from failing to clean", func(t *testing.T) {
		prev := &PRStatus{FailingChecks: []string{"CI"}}
		curr := &PRStatus{}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventCIAllGreen))
	})
	t.Run("from pending to clean", func(t *testing.T) {
		prev := &PRStatus{PendingChecks: []string{"Deploy"}}
		curr := &PRStatus{}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventCIAllGreen))
	})
	t.Run("no transition when already clean", func(t *testing.T) {
		prev := &PRStatus{}
		curr := &PRStatus{}
		assert.Nil(t, findEvent(Diff(prev, curr), EventCIAllGreen))
	})
	t.Run("no green when still failing", func(t *testing.T) {
		prev := &PRStatus{FailingChecks: []string{"CI"}}
		curr := &PRStatus{FailingChecks: []string{"CI"}}
		assert.Nil(t, findEvent(Diff(prev, curr), EventCIAllGreen))
	})
}

func TestDiff_NewUnresolvedThreads(t *testing.T) {
	t.Run("brand new thread", func(t *testing.T) {
		prev := &PRStatus{UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1"}}}}
		curr := &PRStatus{UnresolvedThreads: []ThreadSummary{
			{ID: "T1", CommentIDs: []string{"C1"}},
			{ID: "T2", CommentIDs: []string{"C2"}},
		}}
		e := findEvent(Diff(prev, curr), EventNewUnresolvedThreads)
		require.NotNil(t, e)
		require.Len(t, e.Threads, 1)
		assert.Equal(t, "T2", e.Threads[0].ID)
	})

	t.Run("thread that gained a comment re-fires", func(t *testing.T) {
		prev := &PRStatus{UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1"}}}}
		curr := &PRStatus{UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1", "C2"}}}}
		e := findEvent(Diff(prev, curr), EventNewUnresolvedThreads)
		require.NotNil(t, e)
		require.Len(t, e.Threads, 1)
		assert.Equal(t, "T1", e.Threads[0].ID)
	})

	t.Run("unchanged thread does not fire", func(t *testing.T) {
		prev := &PRStatus{UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1"}}}}
		curr := &PRStatus{UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1"}}}}
		assert.Nil(t, findEvent(Diff(prev, curr), EventNewUnresolvedThreads))
	})

	t.Run("acked thread disappears without firing", func(t *testing.T) {
		// A thread present in prev is absent in curr (it got acked/resolved).
		// That is not a "new" event — curr has nothing new.
		prev := &PRStatus{UnresolvedThreads: []ThreadSummary{{ID: "T1", CommentIDs: []string{"C1"}}}}
		curr := &PRStatus{}
		assert.Empty(t, Diff(prev, curr))
	})
}

func TestDiff_NewGeneralComments(t *testing.T) {
	t.Run("new comment id fires", func(t *testing.T) {
		prev := &PRStatus{GeneralComments: []GeneralComment{{ID: "G1"}}}
		curr := &PRStatus{GeneralComments: []GeneralComment{{ID: "G1"}, {ID: "G2", Author: "alice", Body: "new"}}}
		e := findEvent(Diff(prev, curr), EventNewGeneralComments)
		require.NotNil(t, e)
		require.Len(t, e.Comments, 1)
		assert.Equal(t, "G2", e.Comments[0].ID)
	})

	t.Run("acked comment disappears without firing", func(t *testing.T) {
		prev := &PRStatus{GeneralComments: []GeneralComment{{ID: "G1"}}}
		curr := &PRStatus{}
		assert.Empty(t, Diff(prev, curr))
	})
}

func TestDiff_Conflict(t *testing.T) {
	t.Run("newly conflicting fires", func(t *testing.T) {
		prev := &PRStatus{Conflict: false}
		curr := &PRStatus{Conflict: true}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventConflict))
	})
	t.Run("still conflicting does not re-fire", func(t *testing.T) {
		prev := &PRStatus{Conflict: true}
		curr := &PRStatus{Conflict: true}
		assert.Nil(t, findEvent(Diff(prev, curr), EventConflict))
	})
}

func TestDiff_ReviewTransitions(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		prev := &PRStatus{ReviewDecision: ""}
		curr := &PRStatus{ReviewDecision: "APPROVED", ReviewAuthor: "carol"}
		e := findEvent(Diff(prev, curr), EventReviewApproved)
		require.NotNil(t, e)
		assert.Equal(t, "carol", e.ReviewAuthor)
	})
	t.Run("changes requested", func(t *testing.T) {
		prev := &PRStatus{ReviewDecision: "APPROVED"}
		curr := &PRStatus{ReviewDecision: "CHANGES_REQUESTED", ReviewAuthor: "dave"}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventReviewChangesRequested))
	})
	t.Run("dismissed", func(t *testing.T) {
		prev := &PRStatus{ReviewDecision: "APPROVED"}
		curr := &PRStatus{ReviewDecision: "DISMISSED"}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventReviewDismissed))
	})
	t.Run("dismissed via cleared decision", func(t *testing.T) {
		prev := &PRStatus{ReviewDecision: "CHANGES_REQUESTED"}
		curr := &PRStatus{ReviewDecision: ""}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventReviewDismissed))
	})
	t.Run("no change no event", func(t *testing.T) {
		prev := &PRStatus{ReviewDecision: "APPROVED"}
		curr := &PRStatus{ReviewDecision: "APPROVED"}
		assert.Empty(t, Diff(prev, curr))
	})
	t.Run("pending -> empty does not dismiss", func(t *testing.T) {
		prev := &PRStatus{ReviewDecision: ""}
		curr := &PRStatus{ReviewDecision: ""}
		assert.Nil(t, findEvent(Diff(prev, curr), EventReviewDismissed))
	})
}

func TestDiff_NewCommit(t *testing.T) {
	t.Run("oid change fires with parsed metadata", func(t *testing.T) {
		prev := &PRStatus{LastCommit: CommitSummary{Oid: "aaa111"}}
		curr := &PRStatus{LastCommit: CommitSummary{
			Oid:             "bbb222",
			ShortOid:        "bbb222",
			Author:          "grace",
			Coauthors:       []string{"Ada Lovelace"},
			MessageHeadline: "feat: x",
		}}
		e := findEvent(Diff(prev, curr), EventNewCommit)
		require.NotNil(t, e)
		require.NotNil(t, e.Commit)
		assert.Equal(t, "bbb222", e.Commit.Oid)
		assert.Equal(t, "grace", e.Commit.Author)
		assert.Equal(t, []string{"Ada Lovelace"}, e.Commit.Coauthors)
		assert.Equal(t, "feat: x", e.Commit.MessageHeadline)
	})
	t.Run("same oid no event", func(t *testing.T) {
		prev := &PRStatus{LastCommit: CommitSummary{Oid: "aaa111"}}
		curr := &PRStatus{LastCommit: CommitSummary{Oid: "aaa111"}}
		assert.Nil(t, findEvent(Diff(prev, curr), EventNewCommit))
	})
	t.Run("empty curr oid no event", func(t *testing.T) {
		prev := &PRStatus{LastCommit: CommitSummary{Oid: "aaa111"}}
		curr := &PRStatus{LastCommit: CommitSummary{Oid: ""}}
		assert.Nil(t, findEvent(Diff(prev, curr), EventNewCommit))
	})
}

func TestDiff_StateTransitions(t *testing.T) {
	t.Run("merged", func(t *testing.T) {
		prev := &PRStatus{State: "OPEN", Merged: false}
		curr := &PRStatus{State: "MERGED", Merged: true}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventMerged))
		assert.Nil(t, findEvent(Diff(prev, curr), EventClosed))
	})
	t.Run("closed", func(t *testing.T) {
		prev := &PRStatus{State: "OPEN"}
		curr := &PRStatus{State: "CLOSED"}
		assert.NotNil(t, findEvent(Diff(prev, curr), EventClosed))
	})
	t.Run("no transition when already merged", func(t *testing.T) {
		prev := &PRStatus{State: "MERGED", Merged: true}
		curr := &PRStatus{State: "MERGED", Merged: true}
		assert.Empty(t, Diff(prev, curr))
	})
}

func TestDiffRetrigger(t *testing.T) {
	line := 5
	thread := ThreadSummary{ID: "T1", Path: "a.go", Line: &line, CommentIDs: []string{"C1"}}
	prev := &PRStatus{
		UnresolvedThreads: []ThreadSummary{thread},
		GeneralComments:   []GeneralComment{{ID: "G1", Author: "alice", Body: "hi"}},
	}
	curr := &PRStatus{
		UnresolvedThreads: []ThreadSummary{thread},
		GeneralComments:   []GeneralComment{{ID: "G1", Author: "alice", Body: "hi"}},
	}

	t.Run("plain Diff sees no change", func(t *testing.T) {
		assert.Empty(t, Diff(prev, curr))
	})

	t.Run("retrigger re-emits open thread and comment every poll", func(t *testing.T) {
		events := DiffRetrigger(prev, curr)
		et := findEvent(events, EventNewUnresolvedThreads)
		require.NotNil(t, et)
		assert.Equal(t, "T1", et.Threads[0].ID)
		ec := findEvent(events, EventNewGeneralComments)
		require.NotNil(t, ec)
		assert.Equal(t, "G1", ec.Comments[0].ID)
	})

	t.Run("failing checks still dedup under retrigger", func(t *testing.T) {
		p := &PRStatus{FailingChecks: []string{"CI"}}
		c := &PRStatus{FailingChecks: []string{"CI"}}
		assert.Nil(t, findEvent(DiffRetrigger(p, c), EventNewFailingChecks))
	})

	t.Run("first-poll baseline stays silent under retrigger", func(t *testing.T) {
		assert.Empty(t, DiffRetrigger(nil, curr))
	})
}

// ---------------------------------------------------------------------------
// Annotation tests
// ---------------------------------------------------------------------------

func TestDiff_CheckAnnotations(t *testing.T) {
	t.Run("new annotations on completed check fire once", func(t *testing.T) {
		prev := &PRStatus{
			CheckAnnotations: nil,
		}
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "advisory", Path: "src/main.go", Line: 42, Level: "WARNING", Title: "use of deprecated API", Message: "Foo is deprecated, use Bar instead"},
			},
		}
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e)
		require.NotNil(t, e.Annotations)
		require.Len(t, e.Annotations, 1)
		a := e.Annotations[0]
		assert.Equal(t, "advisory", a.CheckName)
		assert.Equal(t, "src/main.go", a.Path)
		assert.Equal(t, 42, a.Line)
		assert.Equal(t, "WARNING", a.Level)
		assert.Equal(t, "use of deprecated API", a.Title)
		assert.Equal(t, "Foo is deprecated, use Bar instead", a.Message)
	})

	t.Run("unchanged annotations do not re-fire", func(t *testing.T) {
		anns := []AnnotationSummary{
			{CheckName: "advisory", Path: "src/main.go", Line: 42, Level: "WARNING", Title: "deprecated", Message: "..."},
		}
		prev := &PRStatus{CheckAnnotations: anns}
		curr := &PRStatus{CheckAnnotations: anns}
		assert.Nil(t, findEvent(Diff(prev, curr), EventCheckAnnotations))
	})

	t.Run("annotations fire independent of conclusion", func(t *testing.T) {
		// A green check (SUCCESS) with annotations still emits.
		prev := &PRStatus{}
		curr := &PRStatus{
			FailingChecks: []string{},
			PendingChecks: []string{},
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "advisory", Path: "x.go", Line: 1, Level: "WARNING", Title: "t", Message: "m"},
			},
		}
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e, "annotations event must fire even when CI is green")
		assert.Nil(t, findEvent(events, EventNewFailingChecks), "should not fire failing-checks for annotations")
	})

	t.Run("notice-level annotations reach diff unchanged (filtering is at snapshot time)", func(t *testing.T) {
		prev := &PRStatus{}
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "ci", Path: ".github/cache", Line: 0, Level: "NOTICE", Title: "cache miss", Message: "No cache found"},
			},
		}
		// Diff no longer filters by level — that happens at snapshot time.
		// If a NOTICE annotation is in CheckAnnotations, diff will emit it.
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e, "NOTICE-level annotations in CheckAnnotations must be emitted by Diff")
		require.Len(t, e.Annotations, 1)
		assert.Equal(t, "NOTICE", e.Annotations[0].Level)
	})

	t.Run("failure-level annotations are included", func(t *testing.T) {
		prev := &PRStatus{}
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "security", Path: "auth.go", Line: 10, Level: "FAILURE", Title: "vuln", Message: "CVE-..."},
			},
		}
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e, "FAILURE-level annotations should be included")
		require.Len(t, e.Annotations, 1)
		assert.Equal(t, "FAILURE", e.Annotations[0].Level)
	})

	t.Run("first-poll baseline is silent for annotations", func(t *testing.T) {
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "advisory", Path: "x.go", Line: 1, Level: "WARNING", Title: "t", Message: "m"},
			},
		}
		assert.Empty(t, Diff(nil, curr))
	})

	t.Run("multiple annotations from different checks", func(t *testing.T) {
		prev := &PRStatus{}
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "advisory", Path: "a.go", Line: 1, Level: "WARNING", Title: "w1", Message: "m1"},
				{CheckName: "lint", Path: "b.go", Line: 2, Level: "WARNING", Title: "w2", Message: "m2"},
				{CheckName: "security", Path: "c.go", Line: 3, Level: "FAILURE", Title: "w3", Message: "m3"},
			},
		}
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e)
		require.Len(t, e.Annotations, 3)
	})

	t.Run("any annotation in CheckAnnotations produces event (filtering is at snapshot time)", func(t *testing.T) {
		prev := &PRStatus{}
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "ci", Path: "x", Line: 0, Level: "NOTICE", Title: "t", Message: "m"},
			},
		}
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e, "Diff emits whatever annotations are in CheckAnnotations")
		assert.Equal(t, "NOTICE", e.Annotations[0].Level)
	})

	t.Run("same message on different lines are distinct annotations", func(t *testing.T) {
		prev := &PRStatus{}
		curr := &PRStatus{
			CheckAnnotations: []AnnotationSummary{
				{CheckName: "lint", Path: "a.go", Line: 10, Level: "WARNING", Title: "line length", Message: "line too long (120 > 100)"},
				{CheckName: "lint", Path: "a.go", Line: 25, Level: "WARNING", Title: "line length", Message: "line too long (120 > 100)"},
				{CheckName: "lint", Path: "a.go", Line: 42, Level: "WARNING", Title: "line length", Message: "line too long (120 > 100)"},
			},
		}
		events := Diff(prev, curr)
		e := findEvent(events, EventCheckAnnotations)
		require.NotNil(t, e, "annotations identical but for line must all surface")
		require.Len(t, e.Annotations, 3, "three distinct lines = three annotations")
		// Verify lines are preserved correctly.
		lines := make([]int, len(e.Annotations))
		for i, a := range e.Annotations {
			lines[i] = a.Line
		}
		assert.ElementsMatch(t, []int{10, 25, 42}, lines)
	})
}

func TestDiff_MultipleEventsInOnePass(t *testing.T) {
	prev := &PRStatus{
		State:         "OPEN",
		FailingChecks: []string{"CI"},
	}
	curr := &PRStatus{
		State:           "OPEN",
		Conflict:        true,
		FailingChecks:   []string{"CI", "lint"},
		GeneralComments: []GeneralComment{{ID: "G1", Author: "a", Body: "b"}},
		LastCommit:      CommitSummary{Oid: "new"},
	}
	types := eventTypes(Diff(prev, curr))
	assert.Contains(t, types, EventConflict)
	assert.Contains(t, types, EventNewFailingChecks)
	assert.Contains(t, types, EventNewGeneralComments)
	assert.Contains(t, types, EventNewCommit)
}

// ---------------------------------------------------------------------------
// DiffRef tests
// ---------------------------------------------------------------------------

func TestDiffRef_FirstPollBaselineSilent(t *testing.T) {
	curr := &RefStatus{
		Oid:           "abc",
		FailingChecks: []string{"CI"},
		PendingChecks: []string{"Deploy"},
	}
	assert.Empty(t, DiffRef(nil, curr))
}

func TestDiffRef_NewFailingChecks(t *testing.T) {
	prev := &RefStatus{FailingChecks: []string{"CI"}}
	curr := &RefStatus{FailingChecks: []string{"CI", "lint"}}
	e := findEvent(DiffRef(prev, curr), EventNewFailingChecks)
	require.NotNil(t, e)
	assert.Equal(t, []string{"lint"}, e.Checks)
}

func TestDiffRef_CIAllGreen(t *testing.T) {
	prev := &RefStatus{FailingChecks: []string{"CI"}}
	curr := &RefStatus{}
	assert.NotNil(t, findEvent(DiffRef(prev, curr), EventCIAllGreen))
}

func TestDiffRef_NewCommit(t *testing.T) {
	prev := &RefStatus{Oid: "aaa111"}
	curr := &RefStatus{Oid: "bbb222", ShortOid: "bbb222", Author: "grace", MessageHeadline: "fix"}
	e := findEvent(DiffRef(prev, curr), EventNewCommit)
	require.NotNil(t, e)
	require.NotNil(t, e.Commit)
	assert.Equal(t, "bbb222", e.Commit.Oid)
	assert.Equal(t, "grace", e.Commit.Author)
}

func TestDiffRef_MultipleEvents(t *testing.T) {
	prev := &RefStatus{FailingChecks: []string{"CI"}, Oid: "old"}
	curr := &RefStatus{FailingChecks: []string{"CI", "lint"}, Oid: "new", ShortOid: "new", Author: "a"}
	types := eventTypes(DiffRef(prev, curr))
	assert.Contains(t, types, EventNewFailingChecks)
	assert.Contains(t, types, EventNewCommit)
}
