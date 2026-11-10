package presentation

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Tabs uses basic terminal colors and a textual selected marker, so selection
// remains visible when the host strips styles. It performs no terminal query.
func Tabs(names []string, selected, width int) string {
	if width <= 0 {
		return ""
	}
	tabs := make([]string, len(names))
	for i, name := range names {
		text := fmt.Sprintf("%d %s", i+1, flat(name))
		if i == selected {
			text = "[" + text + "]"
			text = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan).Reverse(true).Render(text)
		}
		tabs[i] = text
	}
	return ansi.Truncate(strings.Join(tabs, " "), width, "…")
}
