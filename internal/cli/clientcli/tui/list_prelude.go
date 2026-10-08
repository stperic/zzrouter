package tui

import (
	"fmt"
	"strings"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/stperic/zzrouter/pkg/ui"
)

// ListPrelude carries the ambient state list views share before rendering
// actual rows: loading / error / empty. Views pass their own hint lists so
// the footer matches the view-specific keymap.
type ListPrelude struct {
	// Loading — show spinner. LoadTick feeds shared.SpinnerFrame; LoadingText
	// defaults to "Loading..." when empty.
	Loading     bool
	LoadTick    int
	LoadingText string

	// Err — non-nil renders "Error: ..." styled. Takes precedence over
	// Empty when both set, matching the existing per-view branches.
	Err error

	// Empty — list is loaded but has no items. EmptyText is rendered as-is
	// with a leading space so indentation matches loading/error rows.
	Empty     bool
	EmptyText string

	// BackHint is shown under the loading / error states (typically just
	// "back"). EmptyHint is shown under the empty state (usually "new",
	// "refresh", "back" joined). Pass pre-joined strings produced via
	// joinHints + hint helpers.
	BackHint  string
	EmptyHint string
}

// RenderListPrelude renders one of the three ambient states and returns
// (output, true) so the caller can early-return. Returns ("", false) when
// none of loading / error / empty is set — the caller should proceed with
// the actual row render.
func RenderListPrelude(styles ui.Styles, width int, cfg ListPrelude) (string, bool) {
	var b strings.Builder

	switch {
	case cfg.Loading:
		msg := cfg.LoadingText
		if msg == "" {
			msg = "Loading..."
		}
		b.WriteString(" " + shared.SpinnerFrame(cfg.LoadTick) + " " + msg + "\n")
		b.WriteString("\n" + shared.RenderViewFooter(styles, width, cfg.BackHint))
		return b.String(), true

	case cfg.Err != nil:
		// API errors routinely run past the terminal width; wrap so the tail
		// of the message stays visible instead of being cut at the edge.
		b.WriteString(shared.WrapIndent(styles.Error.Render(fmt.Sprintf("Error: %v", cfg.Err)), " ", width))
		b.WriteString("\n")
		b.WriteString("\n" + shared.RenderViewFooter(styles, width, cfg.BackHint))
		return b.String(), true

	case cfg.Empty:
		msg := cfg.EmptyText
		if msg == "" {
			msg = "No items."
		}
		// Support multi-line empty text so views like Providers can show a
		// short headline followed by help lines without bypassing the
		// prelude. Each line gets the same leading-space indent.
		for _, line := range strings.Split(msg, "\n") {
			b.WriteString(shared.WrapIndent(line, " ", width) + "\n")
		}
		hints := cfg.EmptyHint
		if hints == "" {
			hints = cfg.BackHint
		}
		b.WriteString("\n" + shared.RenderViewFooter(styles, width, hints))
		return b.String(), true
	}

	return "", false
}
