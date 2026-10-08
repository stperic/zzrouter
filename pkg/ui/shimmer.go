package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// ShimmerTickMsg advances the shimmer animation by one frame.
type ShimmerTickMsg struct{}

// ShimmerInterval is the default animation speed.
const ShimmerInterval = 120 * time.Millisecond

// ShimmerTickCmd returns a tea.Cmd that sends a ShimmerTickMsg after the interval.
func ShimmerTickCmd() tea.Cmd {
	return tea.Tick(ShimmerInterval, func(_ time.Time) tea.Msg {
		return ShimmerTickMsg{}
	})
}

// shimmerGradientSize is the number of pre-computed gradient steps.
const shimmerGradientSize = 10

// shimmerPalette holds pre-computed styles for a specific base/bright color pair.
type shimmerPalette struct {
	base, bright string
	styles       [shimmerGradientSize + 1]lipgloss.Style // index 0 = base, index N = bright
}

// cached palette (reused across frames if colors don't change)
var cachedPalette *shimmerPalette

// getShimmerPalette returns a cached palette, rebuilding only if colors changed.
func getShimmerPalette(base, bright string) *shimmerPalette {
	if cachedPalette != nil && cachedPalette.base == base && cachedPalette.bright == bright {
		return cachedPalette
	}

	p := &shimmerPalette{base: base, bright: bright}
	br, bg, bb := hexToRGB(bright)
	dr, dg, db := hexToRGB(base)

	for i := 0; i <= shimmerGradientSize; i++ {
		t := float64(i) / float64(shimmerGradientSize)
		cr := dr + int(float64(br-dr)*t)
		cg := dg + int(float64(bg-dg)*t)
		cb := db + int(float64(bb-db)*t)
		p.styles[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", cr, cg, cb)))
	}

	cachedPalette = p
	return p
}

// Shimmer renders text with a lighter highlight sweeping across it.
// frame is the animation counter. baseColor is the text color (e.g., "#d4a574").
// The highlight is automatically computed as a lighter version of baseColor.
func Shimmer(text string, frame int, baseColor string) string {
	r, g, b := hexToRGB(baseColor)
	// Lighten by blending 50% toward white
	lr := r + (255-r)/2
	lg := g + (255-g)/2
	lb := b + (255-b)/2
	bright := fmt.Sprintf("#%02x%02x%02x", lr, lg, lb)
	return ShimmerText(text, frame, baseColor, bright)
}

// ShimmerText renders text with a moving highlight that sweeps across it.
// frame is the animation counter (increment on each ShimmerTickMsg).
// base and bright are the dim and highlight hex colors (e.g., "#d4a574", "#ead2ba").
func ShimmerText(text string, frame int, base, bright string) string {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return ""
	}

	// The highlight position sweeps across the text
	pos := frame % (n + 4) // +4 gives a small gap between sweeps

	palette := getShimmerPalette(base, bright)

	var b strings.Builder
	for i, r := range runes {
		// Distance from highlight center (0 = brightest)
		dist := abs(i - pos)

		// Map distance to gradient index: 0=bright(10), 1=mostly bright(7), 2=dim(3), 3+=base(0)
		var idx int
		switch {
		case dist == 0:
			idx = shimmerGradientSize // 10 = full bright
		case dist == 1:
			idx = 7
		case dist == 2:
			idx = 3
		default:
			idx = 0 // base
		}

		b.WriteString(palette.styles[idx].Render(string(r)))
	}
	return b.String()
}

func hexToRGB(hex string) (int, int, int) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 128, 128, 128
	}
	var r, g, b int
	_, _ = fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b)
	return r, g, b
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
