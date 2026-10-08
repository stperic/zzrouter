// Package formatting provides shared table formatting utilities for CLI and slash commands.
// This package follows the clean architecture principle of separating presentation logic
// into a reusable shared layer that multiple clients can import.
package formatting

// TableData represents structured table data that can be rendered to different formats.
// This is the common data structure shared between CLI (ASCII) and slash commands (Markdown).
type TableData struct {
	Title   string   // Optional title for the table
	Columns []Column // Column definitions
	Rows    []Row    // Data rows
	Footer  string   // Optional footer text
	Empty   string   // Message to show when there are no rows
}

// Column defines a table column with formatting hints.
type Column struct {
	Header   string // Column header text
	MinWidth int    // Minimum width (0 = auto)
	MaxWidth int    // Maximum width (0 = unlimited)
	Align    Align  // Text alignment
}

// Row represents a single data row in the table.
type Row struct {
	Cells []Cell // Cell values for each column
}

// Cell represents a single cell value with optional formatting.
type Cell struct {
	Value string // Display value
	Raw   any    // Original raw value (for sorting/filtering)
}

// Align specifies text alignment within a column.
type Align int

const (
	AlignLeft Align = iota
	AlignRight
	AlignCenter
)

// NewTableData creates a new TableData with the given columns.
func NewTableData(columns ...Column) *TableData {
	return &TableData{
		Columns: columns,
		Rows:    make([]Row, 0),
		Empty:   "No data available.",
	}
}

// AddRow adds a row of string values to the table.
func (t *TableData) AddRow(values ...string) {
	cells := make([]Cell, len(values))
	for i, v := range values {
		cells[i] = Cell{Value: v, Raw: v}
	}
	t.Rows = append(t.Rows, Row{Cells: cells})
}

// IsEmpty returns true if the table has no rows.
func (t *TableData) IsEmpty() bool {
	return len(t.Rows) == 0
}

// Col is a convenience function to create a Column with just a header.
func Col(header string) Column {
	return Column{Header: header, Align: AlignLeft}
}

// ColRight creates a right-aligned column.
func ColRight(header string) Column {
	return Column{Header: header, Align: AlignRight}
}
