package presentation

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Panel gives every section the same bounded, rounded content surface. Its
// measured frame includes both border rows; content is already safely rendered.
func Panel(title, content string, layout Layout) string {
	if layout.ContentWidth <= 0 || layout.FrameHeight < panelBorderHeight {
		return ""
	}
	inner := layout.ContentWidth
	lines := []string{"╭" + panelCaption(title, inner) + "╮"}
	rows := panelRows(content, inner, layout.BodyHeight)
	for _, row := range rows {
		lines = append(lines, "│"+row+"│")
	}
	lines = append(lines, "╰"+strings.Repeat("─", inner)+"╯")
	return strings.Join(lines, "\n")
}

// SplitPanel draws one shared divider through the entire content height,
// including otherwise empty rows, without wrapping either pane's values.
func SplitPanel(leftTitle, rightTitle, left, right string, layout Layout) string {
	if layout.FrameHeight < panelBorderHeight {
		return ""
	}
	lines := []string{"╭" + panelCaption(leftTitle, layout.ListWidth) + "┬" + panelCaption(rightTitle, layout.DetailWidth) + "╮"}
	leftRows := panelRows(left, layout.ListWidth, layout.BodyHeight)
	rightRows := panelRows(right, layout.DetailWidth, layout.BodyHeight)
	for i, row := range leftRows {
		lines = append(lines, "│"+row+"│"+rightRows[i]+"│")
	}
	lines = append(lines, "╰"+strings.Repeat("─", layout.ListWidth)+"┴"+strings.Repeat("─", layout.DetailWidth)+"╯")
	return strings.Join(lines, "\n")
}

func panelCaption(title string, width int) string {
	if width <= 0 {
		return ""
	}
	caption := ansi.Truncate("─ "+flat(title)+" ", width, "…")
	return accent(caption) + strings.Repeat("─", max(0, width-ansi.StringWidth(caption)))
}

func panelRows(content string, width, height int) []string {
	lines := strings.Split(content, "\n")
	rows := make([]string, max(0, height))
	for i := range rows {
		if i < len(lines) {
			rows[i] = padLine(lines[i], width)
		} else {
			rows[i] = strings.Repeat(" ", max(0, width))
		}
	}
	return rows
}
