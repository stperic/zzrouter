package ui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// Theme holds semantic color slots for the UI.
type Theme struct {
	Name string

	// Core
	Primary   color.Color
	Secondary color.Color
	Accent    color.Color

	// Status
	Success color.Color
	Error   color.Color
	Warning color.Color
	Info    color.Color

	// Text
	Text    color.Color // Main foreground
	Subtext color.Color // Dimmed
	Overlay color.Color // Comments/help

	// Surfaces
	Surface color.Color // Selection bg
	Base    color.Color // Background
	Mantle  color.Color // Darker background

	// Extra
	DetailKey color.Color // Detail keys (Sapphire in Mocha)

	// Derived
	IsDark bool
}

// CatppuccinMocha returns the Catppuccin Mocha dark theme.
func CatppuccinMocha() Theme {
	return Theme{
		Name: "catppuccin-mocha", IsDark: true,
		Primary: lipgloss.Color("#89b4fa"), Secondary: lipgloss.Color("#b4befe"), Accent: lipgloss.Color("#cba6f7"),
		Success: lipgloss.Color("#a6e3a1"), Error: lipgloss.Color("#f38ba8"), Warning: lipgloss.Color("#fab387"), Info: lipgloss.Color("#74c7ec"),
		Text: lipgloss.Color("#cdd6f4"), Subtext: lipgloss.Color("#bac2de"), Overlay: lipgloss.Color("#6c7086"),
		Surface: lipgloss.Color("#313244"), Base: lipgloss.Color("#1e1e2e"), Mantle: lipgloss.Color("#181825"),
		DetailKey: lipgloss.Color("#74c7ec"),
	}
}

// CatppuccinLatte returns the Catppuccin Latte light theme.
func CatppuccinLatte() Theme {
	return Theme{
		Name: "catppuccin-latte", IsDark: false,
		Primary: lipgloss.Color("#1e66f5"), Secondary: lipgloss.Color("#7287fd"), Accent: lipgloss.Color("#8839ef"),
		Success: lipgloss.Color("#40a02b"), Error: lipgloss.Color("#d20f39"), Warning: lipgloss.Color("#fe640b"), Info: lipgloss.Color("#209fb5"),
		Text: lipgloss.Color("#4c4f69"), Subtext: lipgloss.Color("#6c6f85"), Overlay: lipgloss.Color("#9ca0b0"),
		Surface: lipgloss.Color("#ccd0da"), Base: lipgloss.Color("#eff1f5"), Mantle: lipgloss.Color("#e6e9ef"),
		DetailKey: lipgloss.Color("#209fb5"),
	}
}

// TokyoNightTheme returns the Tokyo Night dark theme.
func TokyoNightTheme() Theme {
	return Theme{
		Name: "tokyo-night", IsDark: true,
		Primary: lipgloss.Color("#7aa2f7"), Secondary: lipgloss.Color("#7dcfff"), Accent: lipgloss.Color("#bb9af7"),
		Success: lipgloss.Color("#9ece6a"), Error: lipgloss.Color("#f7768e"), Warning: lipgloss.Color("#ff9e64"), Info: lipgloss.Color("#7aa2f7"),
		Text: lipgloss.Color("#c0caf5"), Subtext: lipgloss.Color("#a9b1d6"), Overlay: lipgloss.Color("#565f89"),
		Surface: lipgloss.Color("#414868"), Base: lipgloss.Color("#1a1b26"), Mantle: lipgloss.Color("#16161e"),
		DetailKey: lipgloss.Color("#7dcfff"),
	}
}

// ClaudeCodeTheme returns a dark theme inspired by Claude Code's terminal UI.
// Warm amber accents on a near-black background with clean, high-contrast text.
func ClaudeCodeTheme() Theme {
	return Theme{
		Name: "claude-code", IsDark: true,
		Primary:   lipgloss.Color("#d4a574"), // warm amber – titles, branding
		Secondary: lipgloss.Color("#7eb8da"), // soft blue – secondary highlights
		Accent:    lipgloss.Color("#c49a6c"), // muted gold – table headers
		Success:   lipgloss.Color("#5fba7d"), // green
		Error:     lipgloss.Color("#e06c75"), // soft red
		Warning:   lipgloss.Color("#e5c07b"), // yellow
		Info:      lipgloss.Color("#7eb8da"), // blue
		Text:      lipgloss.Color("#d4d4d4"), // light gray – main text
		Subtext:   lipgloss.Color("#a0a0a0"), // medium gray – secondary text
		Overlay:   lipgloss.Color("#666666"), // dim gray – separators, faint UI
		Surface:   lipgloss.Color("#2f2f2f"), // selection/highlight bg
		Base:      lipgloss.Color("#1a1a1a"), // background
		Mantle:    lipgloss.Color("#141414"), // darker background (title bar, status)
		DetailKey: lipgloss.Color("#7eb8da"), // blue – detail labels
	}
}

// ResolveTheme picks a theme by name. "auto" detects terminal background.
func ResolveTheme(name string) Theme {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-code", "claude":
		return ClaudeCodeTheme()
	case "catppuccin-mocha", "mocha":
		return CatppuccinMocha()
	case "catppuccin-latte", "latte":
		return CatppuccinLatte()
	case "tokyo-night", "tokyonight":
		return TokyoNightTheme()
	case "auto", "":
		if lipgloss.HasDarkBackground(os.Stdin, os.Stdout) {
			return ClaudeCodeTheme()
		}
		return CatppuccinLatte()
	default:
		return ClaudeCodeTheme() // fallback
	}
}
