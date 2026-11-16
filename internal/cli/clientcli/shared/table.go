package shared

import (
	"fmt"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ProportionalWidth calculates a column width as a fraction of total, with a floor.
func ProportionalWidth(total, numerator, denominator, floor int) int {
	w := (total * numerator) / denominator
	if w < floor {
		return floor
	}
	return w
}

// MinFlexColumnWidth keeps the primary identifier legible when space allows.
const MinFlexColumnWidth = 12

// ColumnLayout describes a table's columns for MeasureColumns.
type ColumnLayout struct {
	Headers []string
	// Rows are the cell values to measure — the full dataset, not just the
	// visible window, so columns don't resize as the list scrolls.
	Rows [][]string
	// Mins floors each column; a nil or short slice floors the rest at 0.
	Mins []int
	// Width is the line budget. Zero — a view rendering before its first
	// WindowSizeMsg — returns pure content widths.
	Width int
	// Pad is the gap added to every column's content width.
	Pad int
	// Flex is the index of the column that shrinks to make the others fit,
	// normally the one holding the longest free-form value.
	Flex int
}

// MeasureColumns sizes table columns to the content they actually hold rather
// than to fixed constants: each column takes its widest cell (header included)
// plus Pad, floored by Mins. The Flex column is then capped so the others fit
// within Width, and the last column absorbs any width left over so dynamic
// trailing content never truncates.
func MeasureColumns(l ColumnLayout) []int {
	widths := make([]int, len(l.Headers))
	copy(widths, l.Mins)

	for col := range widths {
		maxW := lipgloss.Width(l.Headers[col])
		for _, row := range l.Rows {
			if col < len(row) {
				if w := lipgloss.Width(row[col]); w > maxW {
					maxW = w
				}
			}
		}
		if contentW := maxW + l.Pad; contentW > widths[col] {
			widths[col] = contentW
		}
	}

	last := len(widths) - 1
	if l.Width <= 0 || len(widths) == 0 {
		return widths
	}

	// Shrink the flex column so the remaining columns have room.
	if l.Flex >= 0 && l.Flex < last {
		otherCols := 0
		for col, w := range widths {
			if col != l.Flex {
				otherCols += w
			}
		}
		widths[l.Flex] = min(widths[l.Flex], max(l.Width-otherCols, MinFlexColumnWidth))
	}

	// If the flex column is insufficient, shrink the other columns as well.
	total := 0
	for _, w := range widths {
		total += w
	}
	for total > l.Width {
		widest := -1
		for col, w := range widths {
			if w > l.Pad+1 && (widest < 0 || w > widths[widest]) {
				widest = col
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
		total--
	}
	if total < l.Width {
		widths[last] += l.Width - total
	}

	return widths
}

// ScrollTableConfig describes a scrollable table's columns, window, and styling.
// Callers must add only the visible slice of rows: rows[offset : offset+vis].
// The cursor is adjusted internally to be relative to the visible slice.
// rowStyles (when statusCol >= 0) must also be sliced to match: rowStyles[offset : offset+vis].
//
// colWidths: per-column minimum widths. All columns auto-size to content.
// The first column with width 0 is treated as the "flex" column and gets remaining space.
// statusCol: column index to apply per-row status coloring, or -1 to disable.
type ScrollTableConfig struct {
	Headers        []string
	ColumnPriority []int // Higher values keep columns visible longer on narrow terminals.
	ColWidths      []int
	Width          int
	Vis            int
	Cursor         int
	Offset         int
	Styles         ui.Styles
	StatusCol      int              // -1 = no status coloring
	RowStyles      []lipgloss.Style // per-row status styles, sliced to visible range
	AllRows        [][]string       // full dataset for column width measurement (optional)
}

// ScrollTable wraps table construction. Call Row() to add data, then Render().
// The flexible column (colWidths==0) is auto-sized to fit data with a minimum of 40.
type ScrollTable struct {
	cfg  ScrollTableConfig
	rows [][]string
}

func NewScrollTable(cfg ScrollTableConfig) *ScrollTable {
	return &ScrollTable{cfg: cfg}
}

// Row adds a data row to the table.
func (st *ScrollTable) Row(vals ...string) {
	st.rows = append(st.rows, vals)
}

// Render measures the flexible column, builds the lipgloss table, and renders it.
func (st *ScrollTable) Render() string {
	cfg := st.cfg
	helpStyle := cfg.Styles.Help
	selectedStyle := cfg.Styles.Selected
	normalStyle := cfg.Styles.Normal
	cursorIdx := cfg.Cursor - cfg.Offset
	statusCol := cfg.StatusCol
	rowStyles := cfg.RowStyles

	const colPad = 2 // One cell on each side.

	measureRows := cfg.AllRows
	if measureRows == nil {
		measureRows = st.rows
	}

	columns := tableColumns(cfg, measureRows, colPad)
	headers := make([]string, len(columns))
	mins := make([]int, len(columns))
	project := func(row []string) []string {
		out := make([]string, len(columns))
		for i, col := range columns {
			if col < len(row) {
				out[i] = row[col]
			}
		}
		return out
	}
	for i, col := range columns {
		headers[i] = cfg.Headers[col]
		if col < len(cfg.ColWidths) {
			mins[i] = cfg.ColWidths[col]
		}
	}
	projected := make([][]string, len(measureRows))
	for i, row := range measureRows {
		projected[i] = project(row)
	}
	colWidths := MeasureColumns(ColumnLayout{
		Headers: headers,
		Rows:    projected,
		Mins:    mins,
		Width:   cfg.Width,
		Pad:     colPad,
		Flex:    0, // list views put their primary identifier first
	})

	t := table.New().
		Headers(headers...).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderHeader(true).BorderStyle(cfg.Styles.Separator).
		Height(cfg.Vis + TuiTableHeaderAdd).
		Wrap(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().Padding(0, 1)
			if col < len(colWidths) && colWidths[col] > 0 {
				s = s.Width(colWidths[col])
			}
			if row == table.HeaderRow {
				return s.Inherit(helpStyle)
			}
			if row == cursorIdx {
				if statusCol >= 0 && columns[col] == statusCol && row >= 0 && row < len(rowStyles) {
					return s.Inherit(rowStyles[row]).Background(selectedStyle.GetBackground())
				}
				return s.Inherit(selectedStyle)
			}
			if statusCol >= 0 && columns[col] == statusCol && row >= 0 && row < len(rowStyles) {
				return s.Inherit(rowStyles[row])
			}
			return s.Inherit(normalStyle)
		})

	for _, row := range st.rows {
		t.Row(project(row)...)
	}

	// Render without Width() so columns stay tight, then extend each line
	// to full terminal width using styled padding so row highlights span edge to edge.
	raw := t.Render()
	if cfg.Width <= 0 {
		return raw
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		w := lipgloss.Width(line)
		if w >= cfg.Width {
			lines[i] = TruncateText(line, cfg.Width)
			continue
		}
		gap := cfg.Width - w
		// Line 1 is the border separator — extend with ─ characters, not spaces.
		if i == 1 {
			lines[i] = line + Hrule(cfg.Styles, gap)
			continue
		}
		pad := strings.Repeat(" ", gap)
		var padStyle lipgloss.Style
		switch {
		case i == 0:
			padStyle = helpStyle
		case i-2 == cursorIdx:
			padStyle = selectedStyle
		default:
			padStyle = normalStyle
		}
		lines[i] = line + padStyle.Render(pad)
	}
	return strings.Join(lines, "\n")
}

// tableColumns drops the least important columns before identifiers become unreadable.
func tableColumns(cfg ScrollTableConfig, rows [][]string, pad int) []int {
	columns := make([]int, len(cfg.Headers))
	for i := range columns {
		columns[i] = i
	}
	if len(cfg.ColumnPriority) != len(columns) || cfg.Width <= 0 {
		return columns
	}
	widths := MeasureColumns(ColumnLayout{Headers: cfg.Headers, Rows: rows, Mins: cfg.ColWidths, Pad: pad, Flex: 0})
	for len(columns) > 1 {
		needed := 0
		for _, col := range columns {
			floor := max(lipgloss.Width(cfg.Headers[col])+pad, min(widths[col], MinFlexColumnWidth+pad))
			if col < len(cfg.ColWidths) {
				floor = max(floor, cfg.ColWidths[col])
			}
			needed += floor
		}
		if needed <= cfg.Width {
			break
		}
		drop := len(columns) - 1
		for i, col := range columns {
			if cfg.ColumnPriority[col] < cfg.ColumnPriority[columns[drop]] {
				drop = i
			}
		}
		columns = append(columns[:drop], columns[drop+1:]...)
	}
	return columns
}

// TableSliceBounds returns start/end indices for slicing data to the visible window.
func TableSliceBounds(offset, vis, total int) (start, end int) {
	start = offset
	end = min(offset+vis, total)
	return start, end
}

// TableStatus builds the standard "Name (count)  Showing X-Y of Z" status line.
func TableStatus(name string, total, offset, vis int) string {
	if total > vis {
		visEnd := min(offset+vis, total)
		return fmt.Sprintf("Showing %d-%d of %d", offset+1, visEnd, total)
	}
	return fmt.Sprintf("%s (%d)", name, total)
}

// TuiSpinnerFrames is a simple braille dot spinner for list loading
// states. Aliases pkg/ui.SpinnerFrames so the client + server TUIs
// share one canonical frame list.
var TuiSpinnerFrames = ui.SpinnerFrames
