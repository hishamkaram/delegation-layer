// Package presentation renders workbench values without observing or mutating
// application state. The TUI model owns selection and viewport offsets.
package presentation

import "github.com/hishamkaram/delegation-layer/internal/tui/ui"

const (
	selectionWidth      = 2
	columnGap           = 2
	frozenOverflowWidth = 12
	minimumDetailWidth  = 44
	minimumSplitHeight  = 20
)

// Layout is one geometry calculation shared by rendering and viewport sizing.
type Layout struct {
	BodyHeight, ListWidth, DetailWidth int
	Split                              bool
}

// Measure reserves actual chrome heights before allocating the content panes.
func Measure(width, height, headerRows, footerRows int, columns []ui.Column) Layout {
	result := Layout{
		BodyHeight: max(0, height-max(0, headerRows)-max(0, footerRows)),
		ListWidth:  max(0, width),
	}
	natural, valid := naturalWidth(columns)
	if !valid || width < natural+columnGap+minimumDetailWidth || height < minimumSplitHeight {
		return result
	}
	result.Split = true
	result.ListWidth = natural
	result.DetailWidth = width - natural - columnGap
	return result
}

func naturalWidth(columns []ui.Column) (int, bool) {
	if len(columns) == 0 {
		return 0, false
	}
	width := selectionWidth + columnGap*(len(columns)-1)
	for _, column := range columns {
		if column.Width <= 0 {
			return 0, false
		}
		width += column.Width
	}
	return width, true
}

// ClampOffset normalizes the model-owned index among the non-ID columns.
func ClampOffset(columns []ui.Column, width, offset int) int {
	natural, valid := naturalWidth(columns)
	if !valid || width >= natural || len(columns) < 2 {
		return 0
	}
	return min(max(0, offset), len(columns)-2)
}

type visibleColumn struct{ index, width int }

func visibleColumns(columns []ui.Column, width, offset int) []visibleColumn {
	natural, valid := naturalWidth(columns)
	if !valid || width <= 0 {
		return nil
	}
	if width >= natural {
		visible := make([]visibleColumn, len(columns))
		for i, column := range columns {
			visible[i] = visibleColumn{index: i, width: column.Width}
		}
		return visible
	}
	idWidth := min(columns[0].Width, frozenOverflowWidth, max(0, width-selectionWidth))
	visible := []visibleColumn{{index: 0, width: idWidth}}
	used := selectionWidth + idWidth
	for i := ClampOffset(columns, width, offset) + 1; i < len(columns); i++ {
		if used+columnGap+columns[i].Width > width {
			break
		}
		visible = append(visible, visibleColumn{index: i, width: columns[i].Width})
		used += columnGap + columns[i].Width
	}
	return visible
}
