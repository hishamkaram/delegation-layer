package presentation

import (
	"charm.land/lipgloss/v2"

	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

// ListRow shares selection vocabulary with the task table while treating
// legacy section titles/subtitles as neutral, untrusted display text.
func ListRow(title, subtitle string, width int, selected bool) string {
	marker := "  "
	if selected {
		marker = "> "
	}
	return selectionStyle(marker+plainCell(title+"  "+subtitle, max(0, width-selectionWidth)), selected)
}

func accent(text string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan).Render(text)
}

func semantic(text string, role ui.DisplayRole) string {
	style := lipgloss.NewStyle()
	switch role {
	case ui.RolePositive:
		style = style.Foreground(lipgloss.Green)
	case ui.RoleWarning:
		style = style.Foreground(lipgloss.Yellow)
	case ui.RoleNegative:
		style = style.Foreground(lipgloss.Red)
	case ui.RoleNeutral:
	}
	return style.Render(text)
}
