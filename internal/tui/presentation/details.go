package presentation

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

// DetailText derives the full viewport text from the same groups as the preview.
// The legacy field remains a fallback for sections without structured details.
func DetailText(entry ui.Entry) string {
	if len(entry.Details) == 0 {
		return terminal.Safe(entry.Detail)
	}
	groups := make([]string, 0, len(entry.Details))
	for _, group := range entry.Details {
		lines := []string{accent(flat(group.Title) + " ──")}
		for _, field := range group.Fields {
			value := semantic(terminal.Safe(field.Value), field.Role)
			if field.Label != "" {
				value = flat(field.Label) + ": " + value
			}
			lines = append(lines, value)
		}
		groups = append(groups, strings.Join(lines, "\n"))
	}
	return strings.Join(groups, "\n\n")
}

// Details is a bounded preview. The full viewport uses DetailText directly.
func Details(entry ui.Entry, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	text := strings.ReplaceAll(DetailText(entry), "\t", " ")
	lines := strings.Split(ansi.Wrap(text, width, ""), "\n")
	if len(lines) > height {
		lines = append(lines[:height-1], ansi.Truncate("Enter: full details", width, "…"))
	}
	return strings.Join(lines, "\n")
}
