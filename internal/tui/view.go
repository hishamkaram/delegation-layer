package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/tui/presentation"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (m *model) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

// chrome reserves each indicator's row so refreshing never moves the table.
func (m *model) chrome() (string, string) {
	return m.header(), m.footer()
}

func (m *model) header() string {
	names := make([]string, len(m.sections))
	for i, section := range m.sections {
		names[i] = section.Name
	}
	header := presentation.Tabs(names, m.selected, m.width) + "\n" + oneLine("Root: "+m.root, m.width)
	if m.height >= 16 {
		header += "\n" + oneLine("Project: "+m.cwd+" · delegate "+m.version, m.width)
	}
	query := "Search: " + m.appliedSearch + "  (/ edit)"
	if m.pageSearch != m.appliedSearch && (len(m.page.Entries) > 0 || m.page.Summary != "" || len(m.page.Columns) > 0) {
		query = "Previous results · Search: " + m.pageSearch + "  (/ edit request)"
	}
	header += "\n" + oneLine(query, m.width)
	strip := m.page.Strip
	if overflow := presentation.Overflow(m.page.Columns, m.geometryWidth(), m.tableOffset); overflow != "" {
		strip = overflow + " · " + strip
	}
	if m.page.Coverage != "" {
		strip = "c info · " + strip
	}
	if m.height < 16 {
		if status := m.statusText(); status != "" {
			strip = status + " · " + strip
		}
	}
	header += "\n" + oneLine(strip, m.width)
	return header
}

func (m *model) footer() string {
	footer := "Tab views · ↑↓ select · Enter details · a actions · / search · ? help · q quit"
	if m.width < 90 {
		footer = "? help · q quit · a actions · Enter"
	}
	// Compact terminals keep warning rows and one table row visible. Taller
	// terminals reserve the status row even when its values are empty.
	if m.height >= 16 {
		footer = oneLine(m.statusText(), m.width) + "\n" + oneLine(footer, m.width)
	} else {
		footer = oneLine(footer, m.width)
	}
	problem := ""
	if m.problem != "" {
		problem = "Error: " + m.problem
	}
	for _, warning := range []string{problem, restartWarning(m.restartRequired), sourceWarning(m.sourceInvalid)} {
		if warning != "" {
			footer = oneLine(warning, m.width) + "\n" + footer
		}
	}
	return footer
}

func (m *model) statusText() string {
	var parts []string
	if len(m.diagnostics) > 0 || len(m.operationResults) > 0 {
		parts = append(parts, fmt.Sprintf("%d diagnostics (d) · %d results (o)", len(m.diagnostics), len(m.operationResults)))
	}
	if m.pageSearch == m.appliedSearch && (m.page.Next != "" || len(m.cursors) > 0) {
		parts = append(parts, "n next page · p previous page")
	}
	if m.loading {
		parts = append(parts, "Refreshing…")
	}
	return strings.Join(parts, " · ")
}

func restartWarning(required bool) string {
	if required {
		return "RESTART REQUIRED: reopen context"
	}
	return ""
}

func sourceWarning(invalid bool) string {
	if invalid {
		return "SOURCE CHANGED: reopen context"
	}
	return ""
}

func (m *model) geometryWidth() int {
	return presentation.Measure(m.width, m.height, 0, 0, m.page.Columns).ListWidth
}

func (m *model) geometry() presentation.Layout {
	header, footer := m.chrome()
	return presentation.Measure(m.width, m.height, strings.Count(header, "\n")+1, strings.Count(footer, "\n")+1, m.page.Columns)
}

func (m *model) sizeViewport() {
	layout := m.geometry()
	m.viewport.SetWidth(max(1, layout.ContentWidth))
	m.viewport.SetHeight(max(1, layout.BodyHeight-1))
}

func (m *model) render() string {
	if m.width < 38 || m.height < 12 {
		return "Delegate\nTerminal too small. Resize to at least 38 × 12.\nq quit · Ctrl-C quit"
	}
	header, footer := m.chrome()
	layout := m.geometry()
	content := m.body(layout)
	var body string
	if layout.Split && m.listVisible() {
		entry, selected := m.selectedEntry()
		if !selected {
			entry = ui.Entry{Detail: "Select a task to inspect its evidence."}
		}
		details := presentation.Details(entry, layout.DetailWidth, layout.BodyHeight)
		body = presentation.SplitPanel(m.paneTitle(), "Task details", content, details, layout)
	} else {
		body = presentation.Panel(m.paneTitle(), content, layout)
	}
	return fitLines(header+"\n"+body+"\n"+footer, m.width, m.height)
}

func (m *model) listVisible() bool {
	return !m.quitting && m.form == nil && !m.searching && !m.actionMenu && !m.showHelp && !m.detail
}

func (m *model) paneTitle() string {
	switch {
	case m.quitting:
		return "Closing"
	case m.form != nil:
		return m.form.action.Label
	case m.searching:
		return "Search"
	case m.actionMenu:
		return "Actions"
	case m.showHelp:
		return "Help"
	case m.detail:
		return "Details"
	default:
		return m.sections[m.selected].Name
	}
}

func (m *model) body(layout presentation.Layout) string {
	switch {
	case m.quitting:
		if m.forceConfirm {
			return m.pendingText() + "\n\nEnter: force exit and print reconciliation references\nEsc: keep waiting"
		}
		return m.pendingText()
	case m.form != nil:
		return m.form.view(layout.ContentWidth, layout.BodyHeight)
	case m.searching:
		return "Search metadata (not prompts, results or logs)\n" + m.search.View() + "\nEnter apply · Esc back"
	case m.actionMenu:
		return m.actionView(layout)
	case m.showHelp:
		return helpView
	case m.detail:
		return m.viewport.View() + "\nEsc: list · a: actions"
	default:
		return m.listView(layout)
	}
}

func (m *model) listView(layout presentation.Layout) string {
	if len(m.page.Columns) > 0 {
		return presentation.Table(m.page, m.cursor, layout.ListWidth, layout.BodyHeight, m.tableOffset)
	}
	var out strings.Builder
	summary := strings.TrimSpace(ansi.Wrap(terminal.Safe(m.page.Summary), max(1, layout.ContentWidth), ""))
	summaryRows := 0
	if summary != "" {
		out.WriteString(summary + "\n")
		summaryRows = strings.Count(summary, "\n") + 1
	}
	count := max(1, layout.BodyHeight-summaryRows)
	start := max(0, m.cursor-count+1)
	for i := start; i < len(m.page.Entries) && i < start+count; i++ {
		entry := m.page.Entries[i]
		line := presentation.ListRow(entry.Title, entry.Subtitle, layout.ContentWidth, i == m.cursor)
		fmt.Fprintln(&out, line)
	}
	if len(m.page.Entries) == 0 {
		out.WriteString("No records in this view. Open actions to inspect an exact ID or create work.")
	}
	return out.String()
}

func (m *model) actionView(layout presentation.Layout) string {
	var out strings.Builder
	out.WriteString("Actions — Enter open · Esc back\n\n")
	visible := max(1, layout.BodyHeight-2)
	start := max(0, m.actionCursor-visible+1)
	for i := start; i < len(m.actions) && i < start+visible; i++ {
		action := m.actions[i]
		prefix := "  "
		if i == m.actionCursor {
			prefix = "> "
		}
		disabled := ""
		if action.Disabled != "" {
			disabled = " [unavailable: " + action.Disabled + "]"
		}
		fmt.Fprintln(&out, oneLine(prefix+action.Label+disabled, layout.ContentWidth))
	}
	return out.String()
}

func oneLine(s string, width int) string {
	return ansi.Truncate(strings.NewReplacer("\n", " ", "\t", " ").Replace(terminal.Safe(s)), max(1, width), "…")
}

func fitLines(s string, width, height int) string {
	lines := strings.Split(s, "\n")
	height = max(0, height)
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, max(1, width), "…")
	}
	return strings.Join(lines, "\n")
}

const helpView = "Delegate terminal workbench\n\nTab / Shift-Tab or 1–4: switch views\nUp/down or j/k: choose a row; Enter: open details\na: available actions; /: metadata search; r: refresh; d: diagnostics; o: operation results\nn/p: next/previous page; Esc: back\nc: task history coverage; Left/Right: task columns when narrow\nForms: Tab between fields; Ctrl-S review; Enter confirm\nq outside forms or Ctrl-C: quit without cancelling tasks\n\nAutomatic task views read existing evidence only.\nAdmission, liveness and publication are independent.\nGlobal status filters describe dated observations, not current truth.\nRaw logs are private content, opened explicitly and never indexed.\nScheduler serve/run stays in an independent foreground terminal."
