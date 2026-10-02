package backend_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
)

// docsPath is the documentation this package's descriptor generates.
const docsPath = "../docs/BACKENDS.md"

// TestAllCapabilitiesMatchesTheMutationSplit guards the two halves of the
// descriptor against each other: a capability marked Mutation must publish at
// least one wire op, and one that publishes ops must be marked Mutation.
// Without that link the mutation-ops table in the docs would drift from the
// set of capabilities a remote backend is expected to serve.
func TestAllCapabilitiesMatchesTheMutationSplit(t *testing.T) {
	for _, name := range backend.AllCapabilities() {
		info, ok := backend.LookupCapability(name)
		if !ok {
			t.Fatalf("AllCapabilities lists %q but LookupCapability does not know it", name)
		}
		switch {
		case info.Mutation && len(info.Ops) == 0:
			t.Errorf("%s is a mutation capability but publishes no ops", name)
		case !info.Mutation && len(info.Ops) > 0:
			t.Errorf("%s publishes ops but is not a mutation capability", name)
		}
		if info.Interface == "" || info.Summary == "" || info.UsedBy == "" {
			t.Errorf("%s has an incomplete descriptor: %+v", name, info)
		}
	}
}

// TestAllCapabilitiesIsCanonical checks the order is unique and stable, since
// it is what `gh monitor backends` and the remote hello both render from.
func TestAllCapabilitiesIsCanonical(t *testing.T) {
	got := backend.AllCapabilities()
	want := []backend.Capability{
		backend.CapSource, backend.CapReader,
		backend.CapThreads, backend.CapReview,
		backend.CapComments, backend.CapDraft, backend.CapReactions,
		backend.CapReport,
	}
	if len(got) != len(want) {
		t.Fatalf("AllCapabilities() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllCapabilities() = %v, want %v", got, want)
		}
	}
}

// TestLookupCapabilityRejectsUnknown is the check a remote hello's validation
// now rests on: a name outside the descriptor must not resolve, so a server
// announcing it is rejected rather than registered and never resolved.
func TestLookupCapabilityRejectsUnknown(t *testing.T) {
	if _, ok := backend.LookupCapability("checks"); ok {
		t.Fatal(`LookupCapability("checks") = ok, want not found`)
	}
	if _, ok := backend.LookupCapability(""); ok {
		t.Fatal(`LookupCapability("") = ok, want not found`)
	}
}

// TestRegisterAndResolveCapability walks one capability through the untyped
// entry points the remote client uses, including the empty case a server with
// no implementation of its own produces.
func TestRegisterAndResolveCapability(t *testing.T) {
	r := backend.NewRegistry()
	tgt := backend.Target{Kind: backend.KindPR, Owner: "o", Repo: "r", Number: 1}

	if _, _, err := r.ResolveCapability(backend.CapThreads, tgt); err == nil {
		t.Fatal("ResolveCapability on an empty registry = nil error, want ErrNoBackend")
	}

	r.RegisterSource("relay", []backend.Kind{backend.KindPR}, backend.SourceFunc(
		func(context.Context, backend.Target, backend.WatchOptions) (<-chan backend.Update, error) {
			return nil, nil
		}))

	impl, name, err := r.ResolveCapability(backend.CapSource, tgt)
	if err != nil {
		t.Fatalf("ResolveCapability(source) error = %v", err)
	}
	if name != "relay" {
		t.Errorf("ResolveCapability(source) name = %q, want relay", name)
	}
	if _, ok := impl.(backend.Source); !ok {
		t.Errorf("ResolveCapability(source) = %T, want backend.Source", impl)
	}

	// An issue target is outside the relay's kinds, so nothing covers it.
	if _, _, err := r.ResolveCapability(backend.CapSource, backend.Target{Kind: backend.KindIssue}); err == nil {
		t.Error("ResolveCapability(source) on an uncovered kind = nil error, want ErrNoBackend")
	}

	if err := r.RegisterCapability("checks", "relay", nil, nil); err == nil {
		t.Error(`RegisterCapability("checks") = nil error, want unknown capability`)
	}
	if _, _, err := r.ResolveCapability("checks", tgt); err == nil {
		t.Error(`ResolveCapability("checks") = nil error, want unknown capability`)
	}
}

// TestAllEventTypesCoversEveryConstant is the guard the hand-maintained list
// in internal/monitor never had. Every EventType a backend declares must be
// enumerable from one place, so `--events` validation covers a newly declared
// event the day it is declared.
func TestAllEventTypesCoversEveryConstant(t *testing.T) {
	all := backend.AllEventTypes()
	if len(all) == 0 {
		t.Fatal("AllEventTypes() is empty")
	}
	seen := make(map[backend.EventType]bool, len(all))
	for _, e := range all {
		if e == "" {
			t.Error("AllEventTypes() contains an empty event type")
		}
		if seen[e] {
			t.Errorf("AllEventTypes() repeats %q", e)
		}
		seen[e] = true
	}
	// Spot-check both ends of the vocabulary rather than only the count, so a
	// list that silently drops a constant is caught even if a test above is
	// edited to match.
	for _, want := range []backend.EventType{
		backend.EventNewFailingChecks,
		backend.EventCheckAnnotations,
		backend.EventRepoReadiness,
		backend.EventDegraded,
		backend.EventFirstPoll,
		backend.EventAllClear,
	} {
		if !seen[want] {
			t.Errorf("AllEventTypes() is missing %q", want)
		}
	}
}

// TestCapabilityDocsAreCurrent keeps docs/BACKENDS.md honest. Both capability
// tables are generated from the descriptor, so a new capability cannot be
// added without the documentation following it.
func TestCapabilityDocsAreCurrent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(docsPath))
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	doc := string(raw)

	for _, block := range []struct {
		name    string
		content string
	}{
		{"capability-table", renderCapabilityTable()},
		{"ops-table", renderOpsTable()},
	} {
		want := generatedBlock(block.name, block.content)
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not contain the generated %s; regenerate it from backend.CapabilityInfo",
				docsPath, block.name)
		}
	}
}

// renderCapabilityTable renders the "The capabilities" table: every capability
// in canonical order, with its interface, what it does, and the CLI surface
// that exercises it.
func renderCapabilityTable() string {
	rows := [][]string{{"Capability", "Interface", "What it does", "Used by"}}
	for _, name := range backend.AllCapabilities() {
		info, _ := backend.LookupCapability(name)
		rows = append(rows, []string{
			"`" + string(name) + "`",
			"`" + info.Interface + "`",
			info.Summary,
			info.UsedBy,
		})
	}
	return renderTable(rows)
}

// renderOpsTable renders the "Mutation ops" table: the wire operations each
// mutation capability maps to.
func renderOpsTable() string {
	rows := [][]string{{"Capability", "Ops"}}
	for _, name := range backend.AllCapabilities() {
		info, _ := backend.LookupCapability(name)
		if !info.Mutation {
			continue
		}
		quoted := make([]string, 0, len(info.Ops))
		for _, op := range info.Ops {
			quoted = append(quoted, "`"+op+"`")
		}
		rows = append(rows, []string{"`" + string(name) + "`", strings.Join(quoted, ", ")})
	}
	return renderTable(rows)
}

// renderTable formats a Markdown table with every cell padded to its column's
// widest entry, which is what Prettier does. Matching it here keeps
// `prettier --check` and this test from disagreeing about a table that is
// correct either way.
//
// Rows need not agree on their length. A row longer than the header grows the
// column set and a shorter one is padded, so a ragged table renders instead of
// panicking the test binary on an out-of-range width — a capability entry with
// an unexpected cell count is exactly the mistake this helper is here to make
// visible.
func renderTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		if len(row) > len(widths) {
			widths = append(widths, make([]int, len(row)-len(widths))...)
		}
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	line := func(cells []string) string {
		var b strings.Builder
		b.WriteString("|")
		for i := range widths {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			fmt.Fprintf(&b, " %-*s |", widths[i], cell)
		}
		return b.String()
	}
	sep := make([]string, len(widths))
	for i, w := range widths {
		sep[i] = strings.Repeat("-", w)
	}
	out := []string{line(rows[0]), line(sep)}
	for _, row := range rows[1:] {
		out = append(out, line(row))
	}
	return strings.Join(out, "\n")
}

// generatedBlock wraps content in the markers the test looks for. Prettier
// requires a blank line between an HTML comment and the table that follows it,
// so the blank lines here are what keeps `prettier --check` and this test from
// disagreeing about a block that is correct either way.
func generatedBlock(name, content string) string {
	return "<!-- BEGIN generated: " + name + " -->\n\n" + content + "\n\n<!-- END generated: " + name + " -->"
}

// TestRenderTableToleratesRaggedRows pins the contract that a row with more
// cells than the header renders instead of panicking, and that a shorter row
// is padded rather than silently dropping cells. A capability entry with an
// unexpected cell count should fail the docs comparison, not crash the binary.
func TestRenderTableToleratesRaggedRows(t *testing.T) {
	rows := [][]string{
		{"a", "b"},
		{"1", "2", "3"},
		{"4"},
	}
	got := strings.Split(renderTable(rows), "\n")
	// header + separator + one line per data row
	if want := len(rows) + 1; len(got) != want {
		t.Fatalf("got %d rendered lines, want %d", len(got), want)
	}
	for i, line := range got {
		// Every rendered row carries the same cell count as the widest row.
		if n := strings.Count(line, "|"); n != 4 {
			t.Errorf("row %d has %d cell separators, want 4: %q", i, n, line)
		}
	}
	// got[0] header, got[1] separator, got[2] the over-long data row
	if !strings.Contains(got[2], "3") {
		t.Errorf("the over-long row's extra cell was dropped: %q", got[2])
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("renderTable panicked on a ragged table: %v", r)
		}
	}()
	renderTable([][]string{{"only"}})
	renderTable([][]string{{"a", "b", "c"}, {"d"}})
}
