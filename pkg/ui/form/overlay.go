package form

import "github.com/stperic/zzrouter/pkg/ui"

// RenderOverlay frames viewport-sized content using the shared popup renderer.
func RenderOverlay(content, bg string, width, height int, styles ui.Styles) string {
	return ui.RenderOverlayAtWidth(styles, bg, content, width, height, OverlayWidth(width))
}

// OverlayWidth includes the popup border and padding, leaving six cells for chrome.
func OverlayWidth(width int) int { return ui.PopupWidth(width) }
