package ui

import "charm.land/lipgloss/v2"

// BrandASCII is the double-Z mark and Router wordmark in portable ASCII.
const BrandASCII = `     __________
 __________   /
         /   /
     /  /   /   Router
    /  /   /
   /  /________
  /___________`

// BrandWordmark keeps the double-Z identity on a single terminal line.
func BrandWordmark(styles Styles) string {
	accent := "#98472e"
	if styles.Theme.IsDark {
		accent = "#d97757"
	}
	ink := styles.Normal.Bold(true)
	mark := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(accent))
	return ink.Render("Z") + mark.Render("Z") + ink.Render(" Router")
}
