package ui

import "charm.land/lipgloss/v2"

// BrandASCII is the double-Z mark and Router wordmark in portable ASCII.
const BrandASCII = `_________         ____             __
________   /        / __ \____  __  __/ /____  _____
   /   /  /        / /_/ / __ \/ / / / __/ _ \/ ___/
  /   /______     / _, _/ /_/ / /_/ / /_/  __/ /
 /__________     /_/ |_|\____/\__,_/\__/\___/_/`

func brandAccent(styles Styles) lipgloss.Style {
	accent := "#98472e"
	if styles.Theme.IsDark {
		accent = "#d97757"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(accent))
}

// BrandMark renders the shared ASCII logo in the terminal theme.
func BrandMark(styles Styles) string {
	return styles.Normal.Render(BrandASCII)
}

// BrandWordmark keeps working views compact with the plain product name.
func BrandWordmark(styles Styles) string {
	return brandAccent(styles).Bold(true).Render("zz") + styles.Normal.Bold(true).Render("Router")
}
