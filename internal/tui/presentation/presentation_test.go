package presentation

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func taskPage() ui.Page {
	return ui.Page{
		Columns: []ui.Column{
			{Title: "Task ID", Width: 16},
			{Title: "Provider", Width: 18},
			{Title: "Admission", Width: 14},
			{Title: "Liveness", Width: 14},
			{Title: "Publication", Width: 14},
			{Title: "Created", Width: 20},
		},
		Entries: []ui.Entry{
			{ID: strings.Repeat("a", 32), Cells: []ui.Cell{
				{Text: strings.Repeat("a", 32)},
				{Text: "claude:print"},
				{Text: "admitted"},
				{Text: "undetermined"},
				{Text: "committed"},
				{Text: "2026-11-10T07:00:00Z"},
			}},
			{ID: "remote:" + strings.Repeat("b", 32), Cells: []ui.Cell{
				{Text: "remote:" + strings.Repeat("b", 32)},
				{Text: "日本語 provider"},
				{Text: "not observed"},
				{Text: "undetermined", Fault: true},
				{Text: "unknown"},
				{Text: "unknown"},
			}},
		},
	}
}

func TestMeasureUsesOneChromeBudgetAndDerivedSplit(t *testing.T) {
	columns := taskPage().Columns
	for _, tc := range []struct {
		name          string
		width, height int
		split         bool
		list, detail  int
	}{
		{name: "ordinary", width: 120, height: 35, list: 120},
		{name: "before split", width: 153, height: 40, list: 153},
		{name: "split boundary", width: 154, height: 20, split: true, list: 108, detail: 44},
		{name: "short terminal", width: 220, height: 19, list: 220},
		{name: "wide", width: 220, height: 40, split: true, list: 108, detail: 110},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Measure(tc.width, tc.height, 5, 3, columns)
			if got.Split != tc.split || got.ListWidth != tc.list || got.DetailWidth != tc.detail || got.BodyHeight != tc.height-8 {
				t.Fatalf("layout = %+v, want split=%v list=%d detail=%d body=%d", got, tc.split, tc.list, tc.detail, tc.height-8)
			}
		})
	}
	if got := Measure(38, 12, 10, 10, nil); got.BodyHeight != 0 || got.Split {
		t.Fatalf("chrome overflow allocated content: %+v", got)
	}
}

func TestTableAlignsIndependentFieldsByDisplayWidth(t *testing.T) {
	page := taskPage()
	output := ansi.Strip(Table(page, 1, 108, 8, 0))
	lines := strings.Split(output, "\n")
	for row, fields := range [][]string{
		{"Task ID", "Provider", "Admission", "Liveness", "Publication", "Created"},
		{"aaaaaaaaaaaaaaa…", "claude:print", "admitted", "undetermined", "committed", "2026-11-10T07:00:00Z"},
		{"remote:bbbbbbbb…", "日本語 provider", "not observed", "! undetermined", "unknown", "unknown"},
	} {
		starts := []int{2, 20, 40, 56, 72, 88}
		for column, field := range fields {
			// The last row has two literal unknown values; search from the
			// expected preceding field boundary, not the first string match.
			prefix, found := prefixAtDisplayColumn(lines[row], starts[column])
			if !found || !strings.HasPrefix(prefix, field) {
				t.Fatalf("row %d column %d not aligned at %d: %q", row, column, starts[column], lines[row])
			}
		}
	}
	if !strings.HasPrefix(lines[2], "> ") {
		t.Fatalf("selected row marker missing after stripping styles: %q", lines[2])
	}
	if page.Entries[1].ID != "remote:"+strings.Repeat("b", 32) {
		t.Fatal("rendering changed task identity")
	}
}

func prefixAtDisplayColumn(line string, column int) (string, bool) {
	for i := range line {
		if ansi.StringWidth(line[:i]) == column {
			return line[i:], true
		}
	}
	return "", false
}

func TestTableViewportKeepsIdentityAndWholeColumns(t *testing.T) {
	page := taskPage()
	for _, width := range []int{38, 60, 80, 100, 108, 170} {
		for offset := 0; offset < 5; offset++ {
			output := ansi.Strip(Table(page, 1, width, 3, offset))
			assertFits(t, output, width, 3)
			lines := strings.Split(output, "\n")
			if !strings.Contains(lines[0], "Task ID") || !strings.HasPrefix(lines[2], "> remote:") {
				t.Fatalf("width=%d offset=%d lost frozen identity: %q", width, offset, output)
			}
		}
	}
	atStart := ansi.Strip(Table(page, 0, 38, 3, 0))
	atEnd := ansi.Strip(Table(page, 0, 38, 3, 4))
	if !strings.Contains(atStart, "Provider") || strings.Contains(atStart, "Admission") || !strings.Contains(atEnd, "Created") {
		t.Fatalf("narrow table split a column or failed to pan: start=%q end=%q", atStart, atEnd)
	}
	if got := Overflow(page.Columns, 38, 0); got != "+4 ▶" {
		t.Fatalf("start overflow = %q", got)
	}
	if got := Overflow(page.Columns, 38, 4); got != "◀ +4" {
		t.Fatalf("end overflow = %q", got)
	}
	if got := ClampOffset(page.Columns, 108, 4); got != 0 {
		t.Fatalf("wide resize retained hidden columns: offset=%d", got)
	}
	if got := ClampOffset(page.Columns, 38, 99); got != 4 {
		t.Fatalf("oversized offset = %d", got)
	}
}

func TestTableBoundsAndVisibleContractFaults(t *testing.T) {
	page := taskPage()
	page.Entries = append(page.Entries, page.Entries[0], page.Entries[1])
	output := ansi.Strip(Table(page, 3, 108, 2, 0))
	assertFits(t, output, 108, 2)
	if !strings.HasPrefix(strings.Split(output, "\n")[1], "> remote:") {
		t.Fatalf("selected row fell outside viewport: %q", output)
	}
	page.Entries = nil
	if output = ansi.Strip(Table(page, 0, 108, 3, 0)); !strings.Contains(output, "Task ID") || !strings.Contains(output, "No records") {
		t.Fatalf("empty table lost header or empty state: %q", output)
	}
	page.Entries = []ui.Entry{{Cells: []ui.Cell{{Text: "invalid"}}}}
	if output = ansi.Strip(Table(page, 0, 108, 3, 0)); !strings.Contains(output, "cell count mismatch") {
		t.Fatalf("invalid row was silently accepted: %q", output)
	}
	page.Columns[0].Width = 0
	if output = ansi.Strip(Table(page, 0, 80, 3, 0)); !strings.Contains(output, "invalid column widths") {
		t.Fatalf("invalid column was silently accepted: %q", output)
	}
}

func TestUntrustedControlsNeverReachRenderedValues(t *testing.T) {
	poison := "visible\x1b]52;c;clipboard\a\x1b[31mred\x1b[0m\x1b[2J\n\tend\u202e"
	page := taskPage()
	page.Entries[0].Cells[1].Text = poison
	output := Table(page, 0, 108, 3, 0)
	if stripped := ansi.Strip(output); strings.ContainsAny(stripped, "\x1b\a\u202e") || strings.Contains(stripped, "clipboard") || strings.Contains(stripped, "[2J") {
		t.Fatalf("untrusted terminal control survived: %q", output)
	}
	if strings.Contains(output, "\x1b]") || strings.Contains(output, "\x1b[2J") || strings.Contains(output, "\x1b[31m") {
		t.Fatalf("untrusted escape was retained among app styles: %q", output)
	}
	assertFits(t, ansi.Strip(output), 108, 3)
}

func TestDetailsShareStructuredSourceAndBoundPreview(t *testing.T) {
	fullID := "remote:" + strings.Repeat("f", 32)
	entry := ui.Entry{Detail: "obsolete duplicate", Details: []ui.DetailGroup{
		{Title: "Identity", Fields: []ui.DetailField{{Label: "Task", Value: fullID}}},
		{Title: "Evidence", Fields: []ui.DetailField{{Label: "Liveness", Value: "undetermined"}, {Label: "Fault", Value: "previous evidence retained"}}},
	}}
	full := DetailText(entry)
	if !strings.Contains(full, "Task: "+fullID) || !strings.Contains(full, "Evidence\nLiveness: undetermined") || strings.Contains(full, "obsolete duplicate") {
		t.Fatalf("detail source lost structured evidence: %q", full)
	}
	preview := Details(entry, 18, 4)
	assertFits(t, preview, 18, 4)
	if !strings.Contains(preview, "Enter: full detai") {
		t.Fatalf("clipped detail has no full-view hint: %q", preview)
	}
	if got := DetailText(ui.Entry{Detail: "legacy\x1b[2J\nsection"}); got != "legacy\nsection" {
		t.Fatalf("legacy fallback was not sanitized: %q", got)
	}
}

func TestTabsRemainRecognizableWithoutColor(t *testing.T) {
	names := []string{"Tasks", "Teams", "Schedules", "System"}
	output := Tabs(names, 2, 38)
	plain := ansi.Strip(output)
	if plain != "1 Tasks 2 Teams [3 Schedules] 4 System" {
		t.Fatalf("tabs lost navigation or selected marker: %q", plain)
	}
	assertFits(t, plain, 38, 1)
}

func assertFits(t *testing.T, text string, width, height int) {
	t.Helper()
	lines := strings.Split(text, "\n")
	if len(lines) > height {
		t.Fatalf("render has %d lines, height %d: %q", len(lines), height, text)
	}
	for _, line := range lines {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("render line width %d exceeds %d: %q", got, width, line)
		}
	}
}
