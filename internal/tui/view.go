package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
)

func (m *model) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m *model) render() string {
	if m.width < 38 || m.height < 12 {
		return "Delegate\nTerminal too small. Resize to at least 38 × 12.\nq quit · Ctrl-C quit"
	}
	var tabs []string
	for i, section := range m.sections {
		name := fmt.Sprintf("%d %s", i+1, section.Name)
		if i == m.selected {
			name = "[" + name + "]"
		}
		tabs = append(tabs, name)
	}
	header := strings.Join(tabs, "   ") + "\n" + oneLine("Root: "+m.root, m.width) + "\n" + oneLine("Project: "+m.cwd+" · delegate "+m.version, m.width)
	if len(m.diagnostics) > 0 {
		header += fmt.Sprintf(" · %d diagnostics (d)", len(m.diagnostics))
	}
	if len(m.operationResults) > 0 {
		header += fmt.Sprintf(" · %d results (o)", len(m.operationResults))
	}
	body := m.body()
	footer := "Tab views · ↑↓ select · Enter details · a actions · / search · r refresh · ? help · q quit"
	if m.problem != "" {
		footer = "Error: " + terminal.Safe(m.problem) + "\n" + footer
	}
	if m.restartRequired {
		footer = "RESTART REQUIRED: retained cleanup custody; further effects blocked\n" + footer
	}
	if m.sourceInvalid {
		footer = "SOURCE CHANGED: reopen this context before any effect\n" + footer
	}
	bodyHeight := max(1, m.height-5-strings.Count(footer, "\n"))
	return fitLines(header+"\n"+strings.Repeat("─", max(0, m.width))+"\n"+fitLines(body, m.width, bodyHeight)+"\n"+footer, m.width, m.height)
}

func (m *model) body() string {
	switch {
	case m.quitting:
		if m.forceConfirm {
			return m.pendingText() + "\n\nEnter: force exit and print reconciliation references\nEsc: keep waiting"
		}
		return m.pendingText()
	case m.form != nil:
		return m.form.view(m.width-4, m.height-6)
	case m.searching:
		return "Search metadata (not prompts, results or logs)\n" + m.search.View() + "\nEnter apply · Esc back"
	case m.actionMenu:
		return m.actionView()
	case m.showHelp:
		return helpView
	case m.detail:
		return m.viewport.View() + "\nEsc: list · a: actions"
	default:
		return m.listView()
	}
}

func (m *model) listView() string {
	var out strings.Builder
	if m.loading {
		out.WriteString("Refreshing…\n")
	}
	out.WriteString(terminal.Safe(m.page.Summary) + "\n")
	count := max(1, m.height-11)
	start := max(0, m.cursor-count+1)
	listWidth := m.width - 2
	split := m.width >= 110
	if split {
		listWidth = m.width/2 - 4
	}
	for i := start; i < len(m.page.Entries) && i < start+count; i++ {
		entry := m.page.Entries[i]
		prefix := "  "
		if i == m.cursor {
			prefix = "> "
		}
		fmt.Fprintln(&out, oneLine(prefix+entry.Title+"  "+entry.Subtitle, listWidth))
	}
	if len(m.page.Entries) == 0 {
		out.WriteString("No records in this view. Open actions to inspect an exact ID or create work.\n")
	}
	list := out.String()
	if split {
		if entry, ok := m.selectedEntry(); ok {
			detail := lipgloss.NewStyle().Width(m.width - listWidth - 5).MaxHeight(count).Render(terminal.Safe(entry.Detail))
			list = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(listWidth+2).Render(list), "│ ", detail)
		}
	}
	if m.page.Next != "" || len(m.cursors) > 0 {
		list += "\nn next page · p previous page"
	}
	return list
}

func (m *model) actionView() string {
	var out strings.Builder
	out.WriteString("Actions — Enter open · Esc back\n\n")
	visible := max(1, m.height-10)
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
		fmt.Fprintln(&out, oneLine(prefix+action.Label+disabled, m.width-4))
	}
	return out.String()
}

func oneLine(s string, width int) string {
	return ansi.Truncate(strings.NewReplacer("\n", " ", "\t", " ").Replace(terminal.Safe(s)), max(1, width), "…")
}

func fitLines(s string, width, height int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, max(1, width), "…")
	}
	return strings.Join(lines, "\n")
}

const helpView = "Delegate terminal workbench\n\nTab / Shift-Tab or 1–4: switch views\nUp/down or j/k: choose a row; Enter: open details\na: available actions; /: metadata search; r: refresh; d: diagnostics; o: operation results\nn/p: next/previous page; Esc: back\nForms: Tab between fields; Ctrl-S review; Enter confirm\nq outside forms or Ctrl-C: quit without cancelling tasks\n\nAutomatic task views read existing evidence only.\nAdmission, liveness and publication are independent.\nGlobal status filters describe dated observations, not current truth.\nRaw logs are private content, opened explicitly and never indexed.\nScheduler serve/run stays in an independent foreground terminal."
