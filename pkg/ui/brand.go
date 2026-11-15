package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// BrandASCII is the double-Z mark and Router wordmark in portable ASCII.
const BrandASCII = `     __________
 __________   /
         /   /
     /  /   /   Router
    /  /   /
   /  /________
  /___________`

func brandAccent(styles Styles) lipgloss.Style {
	accent := "#98472e"
	if styles.Theme.IsDark {
		accent = "#d97757"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(accent))
}

// BrandMark renders the interlocking monogram using terminal block characters.
func BrandMark(styles Styles) string {
	ink, accent := styles.Normal.Render, brandAccent(styles).Render
	return strings.Join([]string{
		ink("    ▄▄▄▄▄▄▄▄▄▄▄▄▄ "),
		accent(" ▀▀▀▀▀▀▀▀▀██") + ink("   ▟█▀"),
		accent("         ▟█▀") + ink(" ▟█▀  "),
		ink("    ▟█▀") + accent("▟█▀") + ink(" ▟█▀    "),
		ink("  ▟█▀") + accent("▟█▀") + ink(" ▟█▀      "),
		ink("▟█▀ ") + accent("▙▄▄▄▄▄▄▄▄▄▄▄▄ "),
		ink("▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀   "),
	}, "\n")
}

// BrandWordmark keeps working views compact with the plain product name.
func BrandWordmark(styles Styles) string {
	return brandAccent(styles).Bold(true).Render("zz") + styles.Normal.Bold(true).Render("Router")
}
