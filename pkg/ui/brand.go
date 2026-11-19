package ui

import "charm.land/lipgloss/v2"

// BrandASCII is the double-Z mark and Router wordmark in portable ASCII.
const BrandASCII = `_________         ____             __
________   /        / __ \____  __  __/ /____  _____
   /   /  /        / /_/ / __ \/ / / / __/ _ \/ ___/
  /   /______     / _, _/ /_/ / /_/ / /_/  __/ /
 /__________     /_/ |_|\____/\__,_/\__/\___/_/`

const brandTerracotta = "#d97757"

func brandAccent(styles Styles) lipgloss.Style {
	accent := "#98472e"
	if styles.Theme.IsDark {
		accent = brandTerracotta
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(accent))
}

// BrandMark draws the console's interlocking double-Z mark in terminal cells.
func BrandMark(styles Styles) string {
	outer := "#242420"
	if styles.Theme.IsDark {
		outer = "#faf9f5"
	}
	ink := lipgloss.NewStyle().Foreground(lipgloss.Color(outer)).Render
	orange := lipgloss.NewStyle().Foreground(lipgloss.Color(brandTerracotta)).Render
	// Keep the outer strokes even on the quarter-cell grid.
	return " " + ink("▝▀▀▀▀▀▀▀▀▀▀▀██") + "\n" +
		orange("▀▀▀▀▀▀▀▀▙▙") + " " + ink("▟▛▘") + "\n" +
		"   " + ink("▗█▘") + orange("▗▟▀") + ink("▗█▘") + "\n" +
		" " + ink("▗▟▀") + orange("▗▟▛▘") + ink("▐▛") + "\n" +
		ink("▗█▘") + orange("▝▜▛▄▄▄▄▄▄▄▄") + "\n" +
		ink("▐█▄▄▄▄▄▄▄▄▄▄▖")
}

// BrandWordmark keeps working views compact with the plain product name.
func BrandWordmark(styles Styles) string {
	return brandAccent(styles).Bold(true).Render("zz") + styles.Normal.Bold(true).Render("Router")
}
