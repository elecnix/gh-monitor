package monitor

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// hunkHeaderRE matches a unified-diff hunk header, capturing the new-file
// starting line from the `+c,d` component (the `,d` count is optional).
var hunkHeaderRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

const (
	excerptContext = 3  // lines of context on each side of the target
	excerptTail    = 7  // lines shown in the headerless/no-target fallback
	excerptMax     = 20 // hard cap on rendered excerpt lines
)

// formatDiffExcerpt renders a readable window of a unified-diff hunk centered on
// targetLine (the new-file line a thread anchors to). It parses the hunk header
// to track new-file line numbers, walks the body incrementing that counter for
// context/added lines (removed lines don't advance it), and returns ±3 lines
// around the matched line with the target marked ">>> " and the rest indented.
//
// When the header can't be parsed, targetLine is nil, or no line matches, it
// falls back to the last 7 content lines with the final line marked. Empty
// input yields "". The result is capped at ~20 lines.
func formatDiffExcerpt(diffHunk string, targetLine *int) string {
	if strings.TrimSpace(diffHunk) == "" {
		return ""
	}
	lines := strings.Split(diffHunk, "\n")

	newStart := 0
	headerIdx := -1
	for i, l := range lines {
		if m := hunkHeaderRE.FindStringSubmatch(l); m != nil {
			newStart, _ = strconv.Atoi(m[1])
			headerIdx = i
			break
		}
	}

	if headerIdx >= 0 && targetLine != nil {
		type entry struct {
			text    string
			newLine int // 0 for removed lines (no new-file position)
		}
		var entries []entry
		n := newStart
		for _, l := range lines[headerIdx+1:] {
			if strings.HasPrefix(l, "-") {
				entries = append(entries, entry{text: l, newLine: 0})
				continue
			}
			entries = append(entries, entry{text: l, newLine: n})
			n++
		}
		target := -1
		for i := range entries {
			if entries[i].newLine == *targetLine {
				target = i
				break
			}
		}
		if target >= 0 {
			lo := target - excerptContext
			if lo < 0 {
				lo = 0
			}
			hi := target + excerptContext
			if hi > len(entries)-1 {
				hi = len(entries) - 1
			}
			out := make([]string, 0, hi-lo+1)
			for i := lo; i <= hi; i++ {
				out = append(out, markLine(entries[i].text, i == target))
			}
			return capExcerpt(out)
		}
	}

	// Fallback: last few content lines with the final one marked.
	content := lines
	if headerIdx >= 0 {
		content = lines[headerIdx+1:]
	}
	for len(content) > 0 && strings.TrimSpace(content[len(content)-1]) == "" {
		content = content[:len(content)-1]
	}
	if len(content) == 0 {
		return ""
	}
	lo := len(content) - excerptTail
	if lo < 0 {
		lo = 0
	}
	out := make([]string, 0, len(content)-lo)
	for i := lo; i < len(content); i++ {
		out = append(out, markLine(content[i], i == len(content)-1))
	}
	return capExcerpt(out)
}

func markLine(text string, target bool) string {
	if target {
		return ">>> " + text
	}
	return "    " + text
}

func capExcerpt(lines []string) string {
	if len(lines) > excerptMax {
		lines = lines[:excerptMax]
	}
	return strings.Join(lines, "\n")
}

// threadDetail renders an actionable, self-contained body for one unresolved
// thread: its location + latest author, the comment body, a diff excerpt around
// the anchor line, and the two loop-breaking hints (resolve / 👍). It mirrors
// pi-ghpr-monitor's "resolve the thread or react 👍 to stop notifications".
func threadDetail(t ThreadSummary) string {
	loc := t.Path
	if t.Line != nil {
		loc = fmt.Sprintf("%s:%d", t.Path, *t.Line)
	}
	if t.Author != "" {
		loc = fmt.Sprintf("%s (by %s)", loc, t.Author)
	}

	var b strings.Builder
	b.WriteString(loc)
	b.WriteString("\n  ")
	b.WriteString(t.Body)
	if ex := formatDiffExcerpt(t.DiffHunk, t.Line); ex != "" {
		b.WriteString("\n")
		b.WriteString(ex)
	}
	react := "React 👍 to acknowledge: gh monitor react <comment-id> --type thumbs_up"
	if len(t.CommentIDs) > 0 {
		react = fmt.Sprintf("React 👍 to acknowledge: gh monitor react %s --type thumbs_up", t.CommentIDs[len(t.CommentIDs)-1])
	}
	b.WriteString(fmt.Sprintf("\n  Reply then resolve: gh monitor threads resolve --thread-id %s  |  %s", t.ID, react))
	return b.String()
}

// commentDetail renders an actionable body for one general comment.
func commentDetail(c GeneralComment) string {
	return fmt.Sprintf("%s: %s\n  React 👍 to acknowledge and stop notifications: gh monitor react %s --type thumbs_up", c.Author, c.Body, c.ID)
}

// threadsDetail joins the per-thread details with a blank line between them.
func threadsDetail(threads []ThreadSummary) string {
	parts := make([]string, 0, len(threads))
	for _, t := range threads {
		parts = append(parts, threadDetail(t))
	}
	return strings.Join(parts, "\n\n")
}

// commentsDetail joins the per-comment details with a blank line between them.
func commentsDetail(comments []GeneralComment) string {
	parts := make([]string, 0, len(comments))
	for _, c := range comments {
		parts = append(parts, commentDetail(c))
	}
	return strings.Join(parts, "\n\n")
}

// annotationsDetail renders a readable block of check-run annotations.
// When truncated is true, it appends a warning line pointing to the URL
// where the full set is visible.
func annotationsDetail(annotations []AnnotationSummary, truncated bool, url string) string {
	parts := make([]string, 0, len(annotations))
	for _, a := range annotations {
		loc := a.Path
		if a.Line > 0 {
			loc = fmt.Sprintf("%s:%d", a.Path, a.Line)
		}
		parts = append(parts, fmt.Sprintf("%s [%s] %s: %s — %s", a.CheckName, a.Level, loc, a.Title, a.Message))
	}
	result := strings.Join(parts, "\n")
	if truncated {
		if result != "" {
			result += "\n"
		}
		result += fmt.Sprintf("⚠️  Annotation count may be incomplete — %d shown here. See %s for the full set.", len(annotations), url)
	}
	return result
}

// osc8 wraps text in an OSC-8 terminal hyperlink pointing at url.
func osc8(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// LinkifyText returns n.Message with the first occurrence of the PR label
// hyperlinked to the PR URL (when both are set) and, for commit events, the
// commit short oid hyperlinked to the commit URL. Used by the --text output so
// terminals that support OSC-8 render clickable links; others show plain text.
func LinkifyText(n Notification) string {
	msg := n.Message
	if n.PRLabel != "" && n.PRUrl != "" {
		msg = strings.Replace(msg, n.PRLabel, osc8(n.PRUrl, n.PRLabel), 1)
	}
	if n.CommitShortOid != "" && n.CommitUrl != "" {
		msg = strings.Replace(msg, n.CommitShortOid, osc8(n.CommitUrl, n.CommitShortOid), 1)
	}
	return msg
}
