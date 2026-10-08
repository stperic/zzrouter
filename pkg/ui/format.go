package ui

import (
	"strings"
)

// DisplayWidth calculates the display width of a string accounting for emojis
// Most emojis take 2 display columns, ASCII takes 1
func DisplayWidth(s string) int {
	width := 0
	for _, r := range s {
		// Simple heuristic: if rune > 0x1F000, it's likely an emoji (2 columns)
		// Otherwise it's ASCII or similar (1 column)
		if r >= 0x1F000 {
			width += 2
		} else {
			width += 1
		}
	}
	return width
}

// PadRight pads a string to the right with spaces
func PadRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// PadRightDisplayWidth pads a string to the right accounting for emoji display width
// This ensures proper alignment when strings contain emojis
func PadRightDisplayWidth(s string, width int) string {
	displayWidth := DisplayWidth(s)
	if displayWidth >= width {
		return s
	}
	// Add spaces to reach the desired display width
	return s + strings.Repeat(" ", width-displayWidth)
}

// TruncateString truncates a string to maxLen and adds ellipsis
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
