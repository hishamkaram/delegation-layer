package presentation

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

// Table retains the header while keeping the selected row inside the viewport.
// It reads display cells only; Entry.ID is never truncated or used as a cell.
func Table(page ui.Page, cursor, width, height, offset int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	if _, valid := naturalWidth(page.Columns); !valid {
		return plainCell("Table unavailable: invalid column widths", width)
	}
	visible := visibleColumns(page.Columns, width, offset)
	lines := []string{tableHeader(page.Columns, visible, width)}
	if height == 1 {
		return lines[0]
	}
	if height >= 3 {
		lines = append(lines, strings.Repeat("─", width))
	}
	if len(page.Entries) == 0 {
		return strings.Join(append(lines, plainCell("No records in this view.", width)), "\n")
	}
	count := height - len(lines)
	cursor = min(max(0, cursor), len(page.Entries)-1)
	start := max(0, cursor-count+1)
	for i := start; i < len(page.Entries) && i < start+count; i++ {
		lines = append(lines, tableRow(page.Entries[i], len(page.Columns), visible, width, i == cursor))
	}
	return strings.Join(lines, "\n")
}

func tableHeader(columns []ui.Column, visible []visibleColumn, width int) string {
	cells := []string{strings.Repeat(" ", selectionWidth)}
	for _, column := range visible {
		cells = append(cells, plainCell(columns[column.index].Title, column.width))
	}
	line := cells[0] + strings.Join(cells[1:], strings.Repeat(" ", columnGap))
	return accent(padLine(line, width))
}

func tableRow(entry ui.Entry, columnCount int, visible []visibleColumn, width int, selected bool) string {
	marker := "  "
	if selected {
		marker = "> "
	}
	if len(entry.Cells) != columnCount {
		return selectionStyle(plainCell(marker+"Table row unavailable: cell count mismatch", width), selected)
	}
	cells := make([]string, 0, len(visible))
	// Reverse selection inherits the terminal's contrast pair; foreground
	// colors would turn into separate background bars on the selected row.
	for _, column := range visible {
		cell := entry.Cells[column.index]
		cellWidth := column.width
		prefix := ""
		if cell.Fault {
			prefix = plainCell("! ", min(2, cellWidth))
			if !selected {
				prefix = semantic(prefix, ui.RoleNegative)
			}
			cellWidth = max(0, cellWidth-2)
		}
		text := plainCell(cell.Text, cellWidth)
		if !selected {
			text = semantic(text, cell.Role)
		}
		cells = append(cells, prefix+text)
	}
	return selectionStyle(padLine(marker+strings.Join(cells, strings.Repeat(" ", columnGap)), width), selected)
}

// Overflow is placed in chrome rather than taking width from table columns.
func Overflow(columns []ui.Column, width, offset int) string {
	visible := visibleColumns(columns, width, offset)
	if len(visible) == 0 {
		return ""
	}
	left := ClampOffset(columns, width, offset)
	right := len(columns) - 1 - left - (len(visible) - 1)
	var indicators []string
	if left > 0 {
		indicators = append(indicators, fmt.Sprintf("◀ +%d", left))
	}
	if right > 0 {
		indicators = append(indicators, fmt.Sprintf("+%d ▶", right))
	}
	return strings.Join(indicators, " · ")
}

func flat(s string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(terminal.Safe(s))
}

func plainCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return padLine(ansi.Truncate(flat(s), width, "…"), width)
}

func padLine(s string, width int) string {
	s = ansi.Truncate(s, max(0, width), "…")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func selectionStyle(s string, selected bool) string {
	if !selected {
		return s
	}
	return lipgloss.NewStyle().Bold(true).Reverse(true).Render(s)
}
