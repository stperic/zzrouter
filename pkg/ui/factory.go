package ui

// NewRenderer creates a renderer based on options
func NewRenderer(opts RendererOptions) Renderer {
	// Use Catppuccin Mocha theme by default
	theme := opts.Theme
	if theme == nil {
		t := CatppuccinMocha()
		theme = &t
	}

	// Return TUI renderer if interactive mode requested
	if opts.Interactive {
		pageSize := opts.PageSize
		if pageSize <= 0 {
			pageSize = 20 // Default page size
		}
		return NewTUIRenderer(theme, pageSize)
	}

	// Return terminal renderer for CLI mode
	if opts.Adaptive {
		return NewTerminalRendererWithOptions(theme, opts.Adaptive, opts.MinWidth)
	}
	return NewTerminalRenderer(theme)
}
