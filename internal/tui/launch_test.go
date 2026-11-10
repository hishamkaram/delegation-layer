package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func TestHistoryNoticesRemainVisibleWithTableRows(t *testing.T) {
	for _, fixture := range []struct {
		name, description, want string
		problem                 error
	}{
		{name: "degraded cache", problem: errors.New("cache degraded\nprevious rows retained"), want: "cache degraded"},
		{name: "rebuilt cache", description: "Disposable history cache rebuilt; indexing progress is shown below.", want: "Disposable history cache rebuilt"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			m := fixtureModel(t)
			m.page = ui.Page{
				Columns: []ui.Column{{Title: "Task ID", Width: 16}, {Title: "Liveness", Width: 14}},
				Entries: []ui.Entry{{ID: "retained-task", Cells: []ui.Cell{{Text: "retained-task"}, {Text: "undetermined"}}}},
				Strip:   "Dated • discovery incomplete", Coverage: "Full original coverage: dated observations",
			}
			historyNotices(&m.page, fixture.problem, fixture.description)
			if text := ansi.Strip(m.render()); !strings.Contains(text, fixture.want) {
				t.Fatalf("compact table omitted provenance %q: %s", fixture.want, text)
			}
			if strings.Contains(m.page.Strip, "\n") || !strings.Contains(m.page.Strip, "Dated") {
				t.Fatalf("compact notice lost the one-line coverage: %q", m.page.Strip)
			}
			m.browseKey(tea.KeyPressMsg{Code: 'c'})
			if text := ansi.Strip(m.render()); !strings.Contains(text, fixture.want) || !strings.Contains(text, "Full original coverage") {
				t.Fatalf("full coverage omitted provenance or coverage: %s", text)
			}
			if m.page.Entries[0].ID != "retained-task" || m.page.Summary != "" {
				t.Fatalf("notice changed rows or added a second table coverage source: %+v", m.page)
			}
		})
	}
}

func TestHistoryNoticesPreserveUnavailableSummary(t *testing.T) {
	page := ui.Page{Summary: "Exact-ID actions remain available."}
	historyNotices(&page, errors.New("cache unavailable"), "Disposable cache unavailable")
	for _, want := range []string{"Exact-ID actions remain available.", "History unavailable: cache unavailable", "Disposable cache unavailable"} {
		if !strings.Contains(page.Summary, want) {
			t.Fatalf("non-table history lost %q: %+v", want, page)
		}
	}
	if page.Strip != "" || page.Coverage != "" {
		t.Fatalf("unavailable history gained a table presentation: %+v", page)
	}
}
