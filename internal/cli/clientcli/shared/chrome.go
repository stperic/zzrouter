package shared

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/version"
)

// UI chrome height constants — keep in sync with render functions.
const (
	TuiTitleBarLines   = 2 // title bar + separator
	TuiBreadcrumbLines = 2 // breadcrumb + blank line (rendered by parent View)
	TuiTableHeaderAdd  = 2 // table header + separator (added to Height())
	TuiConfirmLines    = 2 // confirmation dialog message + blank
	// TuiTableClickY is the Y offset for mouse clicks: lines above the first data row.
	// title bar(2) + breadcrumb(1) + blank(1) + table header(1) + separator(1) = 6.
	TuiTableClickY = 6
)

// TableTerminator is the 1 newline appended after t.Render() in every list view
// to terminate the last table row. lipgloss table.Render() does not include a
// trailing newline, so all views write t.Render() + "\n".
const TableTerminator = 1

// MaxDetailKeyDivisor caps a detail section's key column at 1/N of the pane.
// Key labels are usually short, but a few sections label rows with an ID or a
// user-supplied metadata key; without a cap one of those would push the values
// it labels into a sliver at the right edge.
const MaxDetailKeyDivisor = 2

// MinWrapWidth is the narrowest value column worth wrapping into. Below it
// the wrapper hard-breaks nearly every word, which is less readable than a
// single overlong line the user can scroll past.
const MinWrapWidth = 8

// EmptyValue is the standard placeholder for empty or missing values.
const EmptyValue = "-"

// NewAltScreenView creates a tea.View configured for alt screen with the theme background.
func NewAltScreenView(content string, theme ui.Theme) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	v.BackgroundColor = theme.Base
	return v
}

// VisibleRows calculates how many data rows fit given a height budget and
// rendered footer string.
// Layout: tableChrome(2) + dataRows + footer. The joining newline adds no row.
// The breadcrumb is rendered by the parent View and already subtracted from height.
func VisibleRows(height int, footer string) int {
	return max(height-TuiTableHeaderAdd-RenderedHeight(footer), 1)
}

// StdListFooter returns a representative footer for standard list views (with status bar).
// Used for height measurement in navigation handlers where the full footer isn't rendered yet.
func StdListFooter(styles ui.Styles) string {
	return RenderViewFooterWithStatus(styles, 80, " ", " ")
}

// Hrule is the single source of truth for horizontal rules across the TUI.
// Every chrome line (title bar, table header separator, footer separators,
// section underlines, search/install/chat rules, quickstart title
// underlines) routes through this helper so there is one rule character,
// one style, and one width knob. Do not reintroduce inline
// strings.Repeat("─", n) anywhere in package cli — the compiler can't catch
// drift there, only grep can.
func Hrule(styles ui.Styles, width int) string {
	if width < 1 {
		return ""
	}
	return styles.Separator.Render(strings.Repeat("─", width))
}

// RenderedHeight counts how many terminal rows a rendered string occupies.
// Each \n terminates a visual line. A trailing partial line (no final \n) also
// counts as one row. An empty string occupies 0 rows.
func RenderedHeight(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// RenderBreadcrumb renders a breadcrumb navigation bar from path segments.
// The last segment is highlighted (bold + primary); ancestors and separators are dimmed.
// Returns exactly TuiBreadcrumbLines (2) lines: breadcrumb + blank.
func RenderBreadcrumb(styles ui.Styles, segments []string) string {
	if len(segments) == 0 {
		return "\n"
	}
	// Padding-free: Title style has Padding(0,1) which would add unwanted spaces between segments.
	active := lipgloss.NewStyle().Bold(true).Foreground(styles.Theme.Primary)
	sep := styles.Help.Render(" / ")
	var parts []string
	for i, seg := range segments {
		if i == len(segments)-1 {
			parts = append(parts, active.Render(seg))
		} else {
			parts = append(parts, styles.Help.Render(seg))
		}
	}
	return " " + strings.Join(parts, sep) + "\n\n"
}

// RenderChildChrome renders the fixed title bar + breadcrumb header
// that precedes any child view's body. Callers (root tuiModel.View and
// the standalone run-logs host) share this so theme/layout drift is
// impossible.
func RenderChildChrome(styles ui.Styles, width int, crumbs []string) string {
	return RenderTitleBar(styles, width) + "\n" + RenderBreadcrumb(styles, crumbs)
}

// renderChildView wraps inner content with the standard alt-screen chrome
// (title bar + breadcrumb header) and returns a tea.View ready for the
// renderer. Each view's View() method calls this so the host doesn't need
// to compose chrome separately.
func RenderChildView(styles ui.Styles, width int, breadcrumb []string, content string) tea.View {
	header := RenderChildChrome(styles, width, breadcrumb)
	v := NewAltScreenView(header+content, styles.Theme)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// breadcrumbClickIndex returns which breadcrumb segment was clicked given X position,
// or -1 if no segment was hit.
func BreadcrumbClickIndex(segments []string, x int) int {
	// Layout: " seg0 / seg1 / seg2" — leading space + segments joined by " / "
	pos := 1 // leading space
	for i, seg := range segments {
		segEnd := pos + lipgloss.Width(seg)
		if x >= pos && x < segEnd {
			return i
		}
		pos = segEnd + 3 // " / " separator
	}
	return -1
}

// renderDetailView renders a scrollable detail view with content and footer.
// The breadcrumb/header is rendered by the parent View; this only renders the body.
// statusLine is shown left-aligned on the indicator row (e.g., deploy status); pass "" to omit.
func RenderDetailView(styles ui.Styles, contentLines []string, scroll int, termHeight int, footer, statusLine string, extraHeaderLines int) (string, int) {
	avail := max(termHeight-extraHeaderLines-TableTerminator-RenderedHeight(footer), 1)

	// Reserve a line for scroll indicator / status when content overflows or status is set
	needsBar := len(contentLines) > avail || statusLine != ""
	if needsBar && avail > 2 {
		avail--
	}

	visible, clampedScroll := ScrollContent(contentLines, scroll, avail)

	var b strings.Builder
	b.WriteString(strings.Join(visible, "\n"))

	if needsBar {
		width := lipgloss.Width(footer)

		// Build scroll arrow
		var arrow string
		if len(contentLines)+1 > avail { // +1 accounts for the bar itself
			hasMore := clampedScroll+avail < len(contentLines)
			hasAbove := clampedScroll > 0
			if hasAbove && hasMore {
				arrow = "▲▼"
			} else if hasMore {
				arrow = " ▼"
			} else {
				arrow = " ▲"
			}
		}

		b.WriteString("\n")
		if statusLine != "" {
			// Status message takes over the bar — no scroll arrows to avoid overlap
			b.WriteString(statusLine)
		} else if arrow != "" {
			pad := max((width-lipgloss.Width(arrow))/2, 0)
			b.WriteString(styles.Help.Render(strings.Repeat(" ", pad) + arrow))
		}
	}

	b.WriteString("\n")
	b.WriteString(footer)
	return b.String(), clampedScroll
}

// TruncateText truncates s to maxWidth display cells, appending "..." if truncated.
// Uses lipgloss.Width for correct handling of emoji and wide characters.
func TruncateText(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	w := lipgloss.Width(s)
	if w <= maxWidth {
		return s
	}
	runes := []rune(s)
	suffix := "..."
	target := maxWidth - 3
	if target <= 0 {
		suffix = ""
		target = maxWidth
	}
	// Binary search for the cut point
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lipgloss.Width(string(runes[:mid])) <= target {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + suffix
}

// WrapText wraps s to width display cells, preserving ANSI styling and
// hard-breaking words too long to fit. Widths below MinWrapWidth return s
// untouched — that includes the zero width views carry before their first
// WindowSizeMsg, where there is no budget to wrap against.
func WrapText(s string, width int) string {
	if width < MinWrapWidth {
		return s
	}
	return lipgloss.Wrap(s, width, "")
}

// WrapIndent wraps s and prefixes every produced line with indent, so a
// wrapped paragraph stays aligned under its first line. width is the total
// line budget including the indent.
func WrapIndent(s, indent string, width int) string {
	lines := strings.Split(WrapText(s, width-lipgloss.Width(indent)), "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

// TruncateRunes clamps s to at most n runes, appending an ellipsis when it
// cuts. Unlike TruncateText it counts runes rather than display cells: it
// backs content caps that bound how much text a pane shows, not columns that
// must fit an exact width.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	const ellipsis = "..."
	if n <= len(ellipsis) {
		return string(r[:n])
	}
	return string(r[:n-len(ellipsis)]) + ellipsis
}

// listNav holds cursor/offset state for any scrollable list view.
// Pass a pointer to cursor/offset and call navigate() with the key msg.
type ListNav struct {
	Cursor *int
	Offset *int
	Total  int
	Vis    int
}

// navigate handles Up/Down/PgUp/PgDn/Home/End for a list view.
// Returns true if a navigation key was matched (caller should return early).
func (n ListNav) Navigate(msg tea.KeyPressMsg, up, down, pgUp, pgDown, home, end key.Binding) bool {
	switch {
	case key.Matches(msg, up):
		if *n.Cursor > 0 {
			*n.Cursor--
			if *n.Cursor < *n.Offset {
				*n.Offset = *n.Cursor
			}
		}
	case key.Matches(msg, down):
		if *n.Cursor < n.Total-1 {
			*n.Cursor++
			if *n.Cursor >= *n.Offset+n.Vis {
				*n.Offset = *n.Cursor - n.Vis + 1
			}
		}
	case key.Matches(msg, pgUp):
		*n.Offset -= n.Vis
		if *n.Offset < 0 {
			*n.Offset = 0
		}
		*n.Cursor = *n.Offset
	case key.Matches(msg, pgDown):
		*n.Offset += n.Vis
		maxOff := max(n.Total-n.Vis, 0)
		if *n.Offset > maxOff {
			*n.Offset = maxOff
		}
		*n.Cursor = *n.Offset
		if *n.Cursor >= n.Total {
			*n.Cursor = n.Total - 1
		}
	case key.Matches(msg, home):
		*n.Cursor, *n.Offset = 0, 0
	case key.Matches(msg, end):
		*n.Cursor = max(n.Total-1, 0)
		*n.Offset = max(n.Total-n.Vis, 0)
	default:
		return false
	}
	return true
}

// detailScroll handles Up/Down/PgUp/PgDn/Home/End for a scrollable detail view.
// Returns true if a navigation key was matched.
func DetailScroll(scroll *int, vis int, msg tea.KeyPressMsg, up, down, pgUp, pgDown, home, end key.Binding) bool {
	switch {
	case key.Matches(msg, up):
		if *scroll > 0 {
			*scroll--
		}
	case key.Matches(msg, down):
		*scroll++
	case key.Matches(msg, pgUp):
		*scroll -= vis
		if *scroll < 0 {
			*scroll = 0
		}
	case key.Matches(msg, pgDown):
		*scroll += vis
	case key.Matches(msg, home):
		*scroll = 0
	case key.Matches(msg, end):
		*scroll = 9999 // clamped by ScrollContent at render time
	default:
		return false
	}
	return true
}

// detailMouseWheel adjusts a detail scroll offset for wheel events.
func DetailMouseWheel(msg tea.MouseWheelMsg, scroll *int) {
	switch msg.Button {
	case tea.MouseWheelUp:
		if *scroll > 0 {
			*scroll--
		}
	case tea.MouseWheelDown:
		*scroll++
	}
}

// mouseWheel adjusts cursor and offset for a scroll wheel event on a list.
// When all items fit on screen, scrolling moves the cursor instead.
// Returns true if the event was handled.
func MouseWheel(msg tea.MouseWheelMsg, cursor, offset *int, total, vis int) bool {
	switch msg.Button {
	case tea.MouseWheelUp:
		if *offset > 0 {
			*offset--
			if *cursor >= *offset+vis {
				*cursor = *offset + vis - 1
			}
		} else if *cursor > 0 {
			*cursor--
		}
	case tea.MouseWheelDown:
		maxOff := max(total-vis, 0)
		if *offset < maxOff {
			*offset++
			if *cursor < *offset {
				*cursor = *offset
			}
		} else if *cursor < total-1 {
			*cursor++
		}
	default:
		return false
	}
	return true
}

// ScrollContent takes fully rendered lines and returns only the visible window.
// scroll is the current offset, avail is the number of visible lines.
// Returns the visible slice and the clamped scroll value.
func ScrollContent(lines []string, scroll, avail int) ([]string, int) {
	if avail <= 0 {
		avail = 1
	}
	maxScroll := max(len(lines)-avail, 0)
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := min(scroll+avail, len(lines))
	return lines[scroll:end], scroll
}

// tableClick maps a mouse click Y coordinate to a data row index for views
// that manage a raw int cursor (off-stack search pane). Views built on
// tui.List should prefer List.Click, which clamps to item bounds and
// skips separators. Both helpers are small and purpose-specific — do not
// introduce a third variant.
func TableClick(msg tea.MouseClickMsg, cursor *int, offset, total, chromeLines int) (enter bool, ok bool) {
	row := msg.Y - chromeLines + offset
	if row < 0 || row >= total {
		return false, false
	}
	if *cursor == row {
		return true, true
	}
	*cursor = row
	return false, true
}

// SpinnerFrame returns the spinner character for the given tick count.
func SpinnerFrame(tick int) string { return ui.SpinnerFrame(tick) }

// StatusStyle returns the appropriate style for a status string.
func StatusStyle(styles ui.Styles, status string) lipgloss.Style {
	switch {
	case strings.HasPrefix(status, "running"), status == "healthy", status == "online",
		strings.HasPrefix(status, "stopping"):
		return styles.StatusRunning
	case strings.HasPrefix(status, "⬇"), strings.HasPrefix(status, "starting"):
		return styles.StatusDownloading
	case status == "failed", status == "cancelled", status == "ERR",
		status == "error", status == "offline":
		return styles.StatusError
	default:
		return styles.StatusIdle
	}
}

// NodeDisplayStatus converts a node's HealthStatus into a display string ("online"/"offline").
func NodeDisplayStatus(healthStatus string) string {
	if healthStatus != "healthy" && healthStatus != "" {
		return "offline"
	}
	return "online"
}

// buildNodeHealthMap builds a map of node name → display status from a slice of nodes.
func BuildNodeHealthMap(nodes []NodeInfo) map[string]string {
	m := make(map[string]string, len(nodes))
	for _, n := range nodes {
		m[n.Name] = NodeDisplayStatus(n.HealthStatus)
	}
	return m
}

// SpinnerTickMsg advances the loading spinner. Aliases
// pkg/ui.SpinnerTickMsg.
type SpinnerTickMsg = ui.SpinnerTickMsg

// SpinnerTickCmd returns a command that sends a SpinnerTickMsg after
// 80ms. Thin wrapper around pkg/ui.SpinnerTickCmd for backwards
// compat with existing client call sites.
func SpinnerTickCmd() tea.Cmd { return ui.SpinnerTickCmd() }

// RenderTitleBar renders the compact brand and version above the separator.
func RenderTitleBar(styles ui.Styles, width int) string {
	if width <= 0 {
		width = 80
	}
	name := " " + ui.BrandWordmark(styles)
	versionLabel := styles.ChatDim.Render("v" + version.Current.ShortString() + " ")
	gap := max(width-lipgloss.Width(name)-lipgloss.Width(versionLabel), 1)
	bar := lipgloss.NewStyle().MaxWidth(width).Render(name + strings.Repeat(" ", gap) + versionLabel)
	return bar + "\n" + Hrule(styles, width)
}

// renderViewFooter renders the standard bottom bar for all views:
// a full-width separator line, optional status line, then key hints.
func RenderViewFooter(styles ui.Styles, width int, hints string) string {
	return RenderViewFooterWithStatus(styles, width, "", hints)
}

// RenderViewFooterWithStatus renders footer: separator + status + separator + hints.
// When status is empty, renders just: separator + hints.
func RenderViewFooterWithStatus(styles ui.Styles, width int, status, hints string) string {
	if width <= 0 {
		width = 80
	}
	sep := Hrule(styles, width) + "\n"
	var b strings.Builder
	b.WriteString(sep)
	if status != "" {
		b.WriteString(" " + styles.HintsBar.Render(TruncateText(strings.ReplaceAll(status, "\n", " "), width-1)) + "\n")
	}
	b.WriteString(" " + styles.HintsBar.Render(FitHints(hints, width-1)))
	return b.String()
}

// FitHints keeps whole shortcuts, including back and help, within one terminal line.
func FitHints(hints string, width int) string {
	if lipgloss.Width(hints) <= width {
		return hints
	}
	parts := strings.Split(hints, "  ")
	var primary, tail []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "Esc ") || strings.HasPrefix(part, "? ") {
			tail = append(tail, part)
		} else {
			primary = append(primary, part)
		}
	}
	selected := []string{}
	for _, part := range primary {
		candidate := strings.Join(append(append([]string{}, selected...), part), "  ")
		if len(tail) > 0 {
			candidate += "  " + strings.Join(tail, "  ")
		}
		if lipgloss.Width(candidate) <= width {
			selected = append(selected, part)
		}
	}
	return TruncateText(strings.Join(append(selected, tail...), "  "), width)
}

// DetailBuilder composes detail-view content. It captures width and styles
// once so callers cannot forget to pass width (previously produced a hardcoded
// 40-char section underline) or misuse fmt-byte padding on ANSI-styled keys
// (previously broke key/value column alignment).
//
// Rows are buffered rather than rendered on the spot: the key column of each
// section is sized to the widest key in THAT section, which is only known once
// the section is complete. Sizing per section rather than per pane keeps a
// section of naturally long keys (a team's key list, a metadata map) from
// pushing every other row's value off to the right.
type DetailBuilder struct {
	styles ui.Styles
	width  int
	ops    []detailOp
	// keyW[i] is the widest rendered key in section i, in display cells.
	keyW    []int
	section int
}

type detailOp struct {
	// raw is pre-rendered content emitted verbatim; when it is non-empty the
	// op is not a field row and key/val/section are unused.
	raw     string
	key     string
	val     string
	section int
}

// NewDetail returns a builder scoped to the given width. Width must match the
// view's content width so section separators and any width-sensitive rows
// extend edge to edge, and so values wrap against the right budget.
func NewDetail(styles ui.Styles, width int) *DetailBuilder {
	return &DetailBuilder{styles: styles, width: width, keyW: []int{0}}
}

// Field adds a " key:   value" row. The key is padded using lipgloss.Width —
// fmt's %-*s counts bytes, which mis-aligns rows when DetailKey.Render adds
// ANSI escape sequences.
//
// A value wider than the remaining space wraps into the value column rather
// than running off the right edge, so long values (model lists, source URLs,
// error strings) stay readable at any terminal width.
func (d *DetailBuilder) Field(key, val string) {
	if val == "" || val == EmptyValue {
		val = EmptyValue
	}
	if w := lipgloss.Width(d.renderKey(key)); w > d.keyW[d.section] {
		d.keyW[d.section] = w
	}
	d.ops = append(d.ops, detailOp{key: key, val: val, section: d.section})
}

// Text adds free-form prose wrapped to the builder width, prefixing every
// produced line with indent. s may already carry styling — the wrapper
// re-emits ANSI state per line.
func (d *DetailBuilder) Text(indent, s string) {
	d.ops = append(d.ops, detailOp{raw: WrapIndent(s, indent, d.width) + "\n"})
}

// Section adds a section header underlined by a full-content-width rule and
// opens a new key-column alignment group.
func (d *DetailBuilder) Section(title string) {
	sepW := max(d.width-2, 20)
	d.ops = append(d.ops, detailOp{raw: "\n" +
		" " + d.styles.DetailKey.Render(title) + "\n" +
		" " + Hrule(d.styles, sepW) + "\n"})
	d.keyW = append(d.keyW, 0)
	d.section = len(d.keyW) - 1
}

// Write appends arbitrary pre-rendered content — for lines that don't match
// the Field or Section shape (status messages, confirmation dialogs,
// helper text, progress bars).
func (d *DetailBuilder) Write(s string) {
	d.ops = append(d.ops, detailOp{raw: s})
}

// renderKey styles a field key. Kept in one place so the width measured when
// the field is added matches the width emitted when it is rendered.
func (d *DetailBuilder) renderKey(key string) string {
	return d.styles.DetailKey.Render(key + ":")
}

// keyColumn returns the key-column width for a section, capped so a single
// long key cannot squeeze the values it labels into a sliver.
func (d *DetailBuilder) keyColumn(section int) int {
	w := d.keyW[section]
	if d.width > 0 {
		return min(w, d.width/MaxDetailKeyDivisor)
	}
	return w
}

// String renders the buffered rows, sizing each section's key column to that
// section's widest key.
func (d *DetailBuilder) String() string {
	var b strings.Builder
	for _, op := range d.ops {
		if op.raw != "" {
			b.WriteString(op.raw)
			continue
		}
		rendered := d.renderKey(op.key)
		pad := max(d.keyColumn(op.section)-lipgloss.Width(rendered), 0)
		// Value column: leading space + key + its padding + one separator space.
		col := 1 + lipgloss.Width(rendered) + pad + 1

		lines := strings.Split(WrapText(op.val, d.width-col), "\n")
		b.WriteString(" " + rendered + strings.Repeat(" ", pad) + " " +
			d.styles.DetailValue.Render(lines[0]) + "\n")
		for _, line := range lines[1:] {
			b.WriteString(strings.Repeat(" ", col) + d.styles.DetailValue.Render(line) + "\n")
		}
	}
	return b.String()
}
