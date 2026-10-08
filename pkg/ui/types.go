package ui

// MessageLevel represents the severity/type of a message
type MessageLevel int

const (
	LevelInfo MessageLevel = iota
	LevelSuccess
	LevelWarning
	LevelError
)

// TableColumn represents a table column with metadata
type TableColumn struct {
	Header   string
	MaxWidth int // Maximum width for truncation (0 = no limit)
}

// TableData represents data to be displayed in a table
type TableData struct {
	Headers []string // Simple headers (backward compatible)
	Rows    [][]string

	// Optional: Enhanced column configuration for adaptive display
	Columns    []TableColumn
	MinColumns int // Minimum number of columns to display (default: all columns)

	// Optional: Row metadata for storing original/full data (e.g., untruncated keys)
	RowMetadata []map[string]any

	// Optional: Title for the table view (TUI mode)
	Title string
}

// Renderer is the base interface for both CLI and TUI display
type Renderer interface {
	// RenderTable displays data in a table format
	RenderTable(data TableData) error

	// ShowMessage displays a message with a specific level
	ShowMessage(msg string, level MessageLevel) error

	// ShowLoading displays a loading indicator with a message
	ShowLoading(msg string) error

	// Close cleans up any resources
	Close() error
}

// InteractiveRenderer extends Renderer for TUI-specific features
type InteractiveRenderer interface {
	Renderer

	// Run starts the interactive event loop
	Run() error

	// Update updates the display with new data
	Update(data any) error

	// SetKeyHandler sets custom key bindings
	SetKeyHandler(handler KeyHandler) error
}

// KeyHandler handles keyboard input in TUI mode
type KeyHandler interface {
	// HandleKey processes a key press for the selected row
	// Returns a tea.Cmd that will produce a message when complete
	HandleKey(selectedIndex int, currentData TableData) any
}

// KeyBinding defines a custom key action in TUI mode
type KeyBinding struct {
	Key         string     // Key to bind (e.g., "r", "d", "enter")
	Description string     // Help text for the key
	Handler     KeyHandler // Handler to call when key is pressed
}

// RendererOptions configures a renderer
type RendererOptions struct {
	Interactive bool
	Theme       *Theme
	PageSize    int
	Adaptive    bool // Enable adaptive column display based on terminal width
	MinWidth    int  // Minimum terminal width (default: 80)
}

// Theme is defined in theme.go as a concrete struct
