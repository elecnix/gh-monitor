package monitor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatDiffExcerpt(t *testing.T) {
	t.Run("parseable header centers on target and marks it", func(t *testing.T) {
		hunk := "@@ -10,5 +10,5 @@ func foo() {\n line10\n line11\n-old\n+new12\n line13"
		// new-file lines: 10=line10, 11=line11, (-old skipped), 12=new12, 13=line13
		out := formatDiffExcerpt(hunk, ptr(12))
		assert.Contains(t, out, ">>> +new12")
		// context lines are indented, not marked
		assert.Contains(t, out, "     line10") // 4-space prefix + " line10"
		// only one line is marked
		assert.Equal(t, 1, strings.Count(out, ">>> "))
	})

	t.Run("window is +/-3 lines around the target", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("@@ -1,20 +1,20 @@\n")
		for i := 1; i <= 20; i++ {
			b.WriteString(" ")
			b.WriteString(lineTok(i))
			b.WriteString("\n")
		}
		out := formatDiffExcerpt(b.String(), ptr(10))
		lines := strings.Split(out, "\n")
		assert.Len(t, lines, 7) // target +/- 3
		assert.Contains(t, out, ">>> "+" "+lineTok(10))
		assert.Contains(t, out, lineTok(7))
		assert.Contains(t, out, lineTok(13))
		assert.NotContains(t, out, lineTok(6))
		assert.NotContains(t, out, lineTok(14))
	})

	t.Run("headerless hunk falls back to the tail", func(t *testing.T) {
		hunk := "not a diff header\nalpha\nbravo\ncharlie"
		out := formatDiffExcerpt(hunk, ptr(3))
		assert.Contains(t, out, ">>> charlie") // final content line marked
		assert.Equal(t, 1, strings.Count(out, ">>> "))
	})

	t.Run("target not found falls back to tail", func(t *testing.T) {
		hunk := "@@ -1,2 +1,2 @@\n line1\n line2"
		out := formatDiffExcerpt(hunk, ptr(999))
		assert.Contains(t, out, ">>> ")
	})

	t.Run("nil target falls back to tail", func(t *testing.T) {
		hunk := "@@ -1,2 +1,2 @@\n line1\n line2"
		out := formatDiffExcerpt(hunk, nil)
		assert.Contains(t, out, ">>>  line2")
	})

	t.Run("empty input yields empty string", func(t *testing.T) {
		assert.Equal(t, "", formatDiffExcerpt("", ptr(1)))
		assert.Equal(t, "", formatDiffExcerpt("   \n  ", ptr(1)))
	})
}

// lineTok returns a distinctive token for line i.
func lineTok(i int) string {
	return "CODE_LINE_" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestRenderNotification_ThreadDetail(t *testing.T) {
	opts := testRunOptions()
	line := 12
	ev := Event{Type: EventNewUnresolvedThreads, Threads: []ThreadSummary{{
		ID:         "T1",
		Path:       "main.go",
		Line:       &line,
		Author:     "alice",
		Body:       "please rename this",
		CommentIDs: []string{"C1"},
		DiffHunk:   "@@ -10,4 +10,4 @@\n line10\n line11\n-old\n+new12\n line13",
	}}}
	n := renderNotificationPR(opts, &PRStatus{}, string(EventNewUnresolvedThreads), ev)
	assert.Contains(t, n.Detail, "main.go:12")
	assert.Contains(t, n.Detail, "(by alice)")
	assert.Contains(t, n.Detail, "please rename this")
	assert.Contains(t, n.Detail, ">>> +new12") // excerpt centered on the anchor line
	assert.Contains(t, n.Detail, "threads resolve --thread-id T1")
	assert.Contains(t, n.Detail, "react C1") // last comment id in the 👍 hint
}

func TestRenderNotification_CommentDetail(t *testing.T) {
	opts := testRunOptions()
	ev := Event{Type: EventNewGeneralComments, Comments: []GeneralComment{
		{ID: "G1", Author: "bob", Body: "can you add a test?"},
		{ID: "G2", Author: "carol", Body: "second thought"},
	}}
	n := renderNotificationPR(opts, &PRStatus{}, string(EventNewGeneralComments), ev)
	assert.Contains(t, n.Detail, "bob: can you add a test?")
	assert.Contains(t, n.Detail, "carol: second thought")
	assert.Contains(t, n.Detail, "react G1 --type thumbs_up")
}

func TestAnnotationsDetail(t *testing.T) {
	t.Run("non-truncated renders annotations without warning", func(t *testing.T) {
		anns := []AnnotationSummary{
			{CheckName: "lint", Path: "x.go", Line: 1, Level: "WARNING", Title: "unused", Message: "var x is unused"},
		}
		out := annotationsDetail(anns, false, "")
		assert.Contains(t, out, "lint [WARNING] x.go:1: unused — var x is unused")
		assert.NotContains(t, out, "incomplete")
		assert.NotContains(t, out, "⚠️")
	})

	t.Run("truncated appends warning with URL", func(t *testing.T) {
		anns := []AnnotationSummary{
			{CheckName: "lint", Path: "x.go", Line: 1, Level: "WARNING", Title: "unused", Message: "var x is unused"},
		}
		out := annotationsDetail(anns, true, "https://github.com/o/r/actions/runs/1")
		assert.Contains(t, out, "⚠️")
		assert.Contains(t, out, "incomplete")
		assert.Contains(t, out, "https://github.com/o/r/actions/runs/1")
		assert.Contains(t, out, "1 shown here")
	})

	t.Run("truncated with empty annotations still shows warning", func(t *testing.T) {
		out := annotationsDetail(nil, true, "https://github.com/o/r/actions/runs/1")
		assert.Contains(t, out, "⚠️")
		assert.Contains(t, out, "incomplete")
		assert.Contains(t, out, "0 shown here")
	})
}

func TestLinkifyText(t *testing.T) {
	t.Run("wraps pr label", func(t *testing.T) {
		n := Notification{
			Message: "❌ Failing CI checks on o/r#7: build",
			PRLabel: "o/r#7",
			PRUrl:   "https://github.com/o/r/pull/7",
		}
		out := LinkifyText(n)
		require.Contains(t, out, "\x1b]8;;https://github.com/o/r/pull/7\x1b\\o/r#7\x1b]8;;\x1b\\")
		// surrounding text preserved
		assert.Contains(t, out, "❌ Failing CI checks on ")
		assert.Contains(t, out, ": build")
	})

	t.Run("wraps commit short oid too", func(t *testing.T) {
		n := Notification{
			Message:        "📝 New commit abc1234 pushed to o/r#7",
			PRLabel:        "o/r#7",
			PRUrl:          "https://github.com/o/r/pull/7",
			CommitShortOid: "abc1234",
			CommitUrl:      "https://github.com/o/r/commit/abc1234def",
		}
		out := LinkifyText(n)
		assert.Contains(t, out, "\x1b]8;;https://github.com/o/r/commit/abc1234def\x1b\\abc1234\x1b]8;;\x1b\\")
		assert.Contains(t, out, "\x1b]8;;https://github.com/o/r/pull/7\x1b\\o/r#7\x1b]8;;\x1b\\")
	})

	t.Run("no urls leaves message untouched", func(t *testing.T) {
		n := Notification{Message: "plain o/r#7", PRLabel: "o/r#7"}
		assert.Equal(t, "plain o/r#7", LinkifyText(n))
	})
}
