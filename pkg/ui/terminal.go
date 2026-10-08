package ui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"
)

// Constants for terminal rendering
const (
	TabSpacing        = 2   // Spacing between columns
	SafetyMargin      = 5   // Safety margin for terminal width calculations
	DefaultMinWidth   = 80  // Default minimum terminal width
	DefaultMaxWidth   = 120 // Default maximum terminal width (fallback)
	TabWriterMinWidth = 0   // Minimum cell width for tabwriter
	TabWriterTabWidth = 0   // Tab width for tabwriter
	TabWriterPadding  = 2   // Padding for tabwriter
	TabWriterPadChar  = ' ' // Padding character for tabwriter
	TabWriterFlags    = 0   // Flags for tabwriter
)

// TerminalRenderer implements Renderer for terminal/CLI output
type TerminalRenderer struct {
	theme    *Theme
	writer   *tabwriter.Writer
	adaptive bool
	minWidth int
}

// NewTerminalRenderer creates a new terminal renderer with default settings
func NewTerminalRenderer(theme *Theme) *TerminalRenderer {
	return NewTerminalRendererWithOptions(theme, false, DefaultMinWidth)
}

// NewTerminalRendererWithOptions creates a terminal renderer with custom options
func NewTerminalRendererWithOptions(theme *Theme, adaptive bool, minWidth int) *TerminalRenderer {
	if theme == nil {
		t := CatppuccinMocha()
		theme = &t
	}
	if minWidth <= 0 {
		minWidth = DefaultMinWidth
	}
	return &TerminalRenderer{
		theme:    theme,
		writer:   tabwriter.NewWriter(os.Stdout, TabWriterMinWidth, TabWriterTabWidth, TabWriterPadding, TabWriterPadChar, TabWriterFlags),
		adaptive: adaptive,
		minWidth: minWidth,
	}
}

// RenderTable displays data in a table format
func (r *TerminalRenderer) RenderTable(data TableData) error {
	// Use adaptive rendering if enabled and columns are configured
	if r.adaptive && len(data.Columns) > 0 {
		return r.renderAdaptiveTable(data)
	}

	// Fallback to simple rendering
	return r.renderSimpleTable(data)
}

// renderSimpleTable renders a table without adaptive column handling
func (r *TerminalRenderer) renderSimpleTable(data TableData) error {
	// Validate input
	if len(data.Headers) == 0 {
		return nil // Nothing to render
	}

	// Print headers
	if _, err := fmt.Fprintln(r.writer, strings.Join(data.Headers, "\t")); err != nil {
		return fmt.Errorf("failed to write headers: %w", err)
	}

	// Print separator - dynamically sized based on header width
	separators := make([]string, len(data.Headers))
	for i, header := range data.Headers {
		separators[i] = strings.Repeat("-", len(header))
	}
	if _, err := fmt.Fprintln(r.writer, strings.Join(separators, "\t")); err != nil {
		return fmt.Errorf("failed to write separators: %w", err)
	}

	// Print rows
	for rowIdx, row := range data.Rows {
		if _, err := fmt.Fprintln(r.writer, strings.Join(row, "\t")); err != nil {
			return fmt.Errorf("failed to write row %d: %w", rowIdx, err)
		}
	}

	// Flush the table
	if err := r.writer.Flush(); err != nil {
		return err
	}

	// Always add empty line after output before command prompt
	_, _ = fmt.Fprintln(os.Stdout)

	return nil
}

// renderAdaptiveTable renders a table with adaptive column display
func (r *TerminalRenderer) renderAdaptiveTable(data TableData) error {
	// Validate input
	if len(data.Columns) == 0 {
		return nil // Nothing to render
	}

	// Get terminal width
	width := r.getTerminalWidth()

	// Determine which columns to show based on width and minimum columns
	visibleColumns := r.selectVisibleColumnsWithData(data.Columns, data.Rows, width, data.MinColumns)

	// Build headers for visible columns
	headers := make([]string, len(visibleColumns))
	for i, colIdx := range visibleColumns {
		headers[i] = data.Columns[colIdx].Header
	}

	// Calculate max width for each visible column (using display width)
	columnWidths := make([]int, len(visibleColumns))
	for i, colIdx := range visibleColumns {
		// Start with header width
		columnWidths[i] = DisplayWidth(data.Columns[colIdx].Header)
		// Check all rows for max width
		for _, row := range data.Rows {
			if colIdx < len(row) {
				cellWidth := DisplayWidth(row[colIdx])
				if cellWidth > columnWidths[i] {
					columnWidths[i] = cellWidth
				}
			}
		}
	}

	// Manually format and print headers (no tabwriter for emoji support)
	var headerLine strings.Builder
	for i, header := range headers {
		if i > 0 {
			headerLine.WriteString("  ") // 2 spaces between columns
		}
		// Pad header to column width
		headerLine.WriteString(PadRight(header, columnWidths[i]))
	}
	if _, err := fmt.Fprintln(r.writer, headerLine.String()); err != nil {
		return fmt.Errorf("failed to write headers: %w", err)
	}

	// Print separator
	var separatorLine strings.Builder
	for i, width := range columnWidths {
		if i > 0 {
			separatorLine.WriteString("  ")
		}
		separatorLine.WriteString(strings.Repeat("-", width))
	}
	if _, err := fmt.Fprintln(r.writer, separatorLine.String()); err != nil {
		return fmt.Errorf("failed to write separator: %w", err)
	}

	// Print rows
	for _, row := range data.Rows {
		var rowLine strings.Builder
		for i, colIdx := range visibleColumns {
			if i > 0 {
				rowLine.WriteString("  ")
			}
			cell := ""
			if colIdx < len(row) {
				cell = row[colIdx]
				// Apply truncation if MaxWidth is set
				if data.Columns[colIdx].MaxWidth > 0 {
					cell = TruncateString(cell, data.Columns[colIdx].MaxWidth)
				}
			}
			// Pad cell to column width using display-aware padding
			rowLine.WriteString(PadRightDisplayWidth(cell, columnWidths[i]))
		}
		if _, err := fmt.Fprintln(r.writer, rowLine.String()); err != nil {
			return fmt.Errorf("failed to write row: %w", err)
		}
	}

	// Show note if some columns were hidden (as last row in table)
	if len(visibleColumns) < len(data.Columns) {
		if _, err := fmt.Fprintln(r.writer); err != nil {
			return fmt.Errorf("failed to write blank line: %w", err)
		}
		if _, err := fmt.Fprintln(r.writer, "Note: Not all columns displayed due to terminal width constraints."); err != nil {
			return fmt.Errorf("failed to write note: %w", err)
		}
	}

	// Flush the table
	if err := r.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush table: %w", err)
	}

	// Always add empty line after output before command prompt
	_, _ = fmt.Fprintln(os.Stdout)

	return nil
}

// getTerminalWidth returns the current terminal width with safety checks
func (r *TerminalRenderer) getTerminalWidth() int {
	// Check if stdout is a terminal (not redirected)
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return DefaultMaxWidth
	}

	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		// Fallback to default width if detection fails
		return DefaultMaxWidth
	}

	// Check for COLUMNS environment variable (more reliable in some terminals)
	if colsEnv := os.Getenv("COLUMNS"); colsEnv != "" {
		if cols, err := strconv.Atoi(colsEnv); err == nil && cols > 0 && cols < width {
			width = cols
		}
	}

	// Ensure width is at least minWidth (only if minWidth is set)
	if r.minWidth > 0 && width < r.minWidth {
		return r.minWidth
	}

	return width
}

// selectVisibleColumnsWithData determines which columns to show based on terminal width
// It fits as many columns as possible from left to right, respecting MinColumns
func (r *TerminalRenderer) selectVisibleColumnsWithData(columns []TableColumn, rows [][]string, termWidth int, minColumns int) []int {
	// Validate inputs
	if len(columns) == 0 {
		return []int{}
	}
	if termWidth <= 0 {
		termWidth = DefaultMaxWidth
	}
	if minColumns <= 0 || minColumns > len(columns) {
		minColumns = len(columns) // Default: try to show all columns
	}

	// Calculate actual width needed for each column (max of header and data)
	columnWidths := make([]int, len(columns))
	for i, col := range columns {
		// Start with header display width (accounting for emojis)
		columnWidths[i] = DisplayWidth(col.Header)

		// Check all rows for this column
		for _, row := range rows {
			if i < len(row) {
				cellWidth := DisplayWidth(row[i])
				// Apply MaxWidth truncation if set
				if col.MaxWidth > 0 && cellWidth > col.MaxWidth {
					cellWidth = col.MaxWidth
				}
				if cellWidth > columnWidths[i] {
					columnWidths[i] = cellWidth
				}
			}
		}
	}

	// Try to fit columns from left to right
	visible := []int{}
	currentWidth := 0

	for i := range columns {
		// Calculate width if we add this column
		testWidth := currentWidth
		if currentWidth > 0 {
			testWidth += TabSpacing // Add spacing before column
		}
		testWidth += columnWidths[i]

		// Check if it fits or if it's a required column
		if testWidth <= (termWidth-SafetyMargin) || i < minColumns {
			currentWidth = testWidth
			visible = append(visible, i)
		} else {
			// Stop adding columns once we exceed width (and past minimum)
			break
		}
	}

	return visible
}

// ShowMessage displays a message with a specific level
func (r *TerminalRenderer) ShowMessage(msg string, level MessageLevel) error {
	prefix := ""
	switch level {
	case LevelSuccess:
		prefix = "✓ "
	case LevelError:
		prefix = "✗ "
	case LevelWarning:
		prefix = "⚠ "
	case LevelInfo:
		prefix = "ℹ "
	}
	fmt.Println(prefix + msg)
	return nil
}

// ShowLoading displays a loading indicator
func (r *TerminalRenderer) ShowLoading(msg string) error {
	fmt.Printf("⏳ %s...\n", msg)
	return nil
}

// Close cleans up resources
func (r *TerminalRenderer) Close() error {
	if r.writer != nil {
		return r.writer.Flush()
	}
	return nil
}
