package form

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/stperic/zzrouter/pkg/ui"
)

// ChecklistField is a filter-first multi-select.
//
// Blurred: renders a one-line summary. When focused, expands in place into
// a bordered inner box with (1) a filter line, (2) a fixed-height viewport
// of visible options, and (3) a hint line. Scales from 5 to ~200 items —
// filter carries the UX at high counts.
//
// Keybindings (when focused, list mode):
//
//	↑ / ↓        move cursor (hands off to sibling field at edge)
//	space        toggle item under cursor
//	/            enter filter mode
//	a            select all visible
//	n            deselect all visible
//	i            invert visible
//	Home / End   jump to first / last visible
//	PgUp / PgDn  page by viewport size
//	esc          clear filter (or defer to outer view if no filter)
//
// Keybindings (filter mode):
//
//	printable    append to filter
//	backspace    remove last char (empties → exits filter mode)
//	enter / esc  exit filter mode, keep filter text
//	↑ / ↓        exit filter mode and move cursor
//	space        still toggles the cursor item (filter does not accept spaces)
type ChecklistField struct {
	label     string
	options   []string
	selected  map[string]bool
	emptyHint string
	allHint   string
	focused   bool

	// Viewport / cursor state. cursor indexes into the FILTERED list.
	cursor        int
	viewportStart int
	viewportSize  int

	// Filter state. filter is the current substring query; filterActive
	// means keystrokes go to the filter input rather than the list.
	filter       string
	filterActive bool
}

// NewChecklist builds a checklist over the given options. emptyHint is
// rendered in place of the summary when there are no options at all;
// allHint is shown when the options are non-empty but nothing is selected.
func NewChecklist(label string, options []string, emptyHint, allHint string) *ChecklistField {
	return &ChecklistField{
		label:        label,
		options:      options,
		selected:     make(map[string]bool),
		emptyHint:    emptyHint,
		allHint:      allHint,
		viewportSize: 8,
	}
}

// SetOptions replaces the option list. Previously-selected entries that
// remain in the new list keep their selection; others are dropped. Cursor
// and viewport reset.
func (f *ChecklistField) SetOptions(options []string) {
	keep := make(map[string]bool, len(f.selected))
	for _, o := range options {
		if f.selected[o] {
			keep[o] = true
		}
	}
	f.options = options
	f.selected = keep
	f.cursor = 0
	f.viewportStart = 0
	f.filter = ""
	f.filterActive = false
}

// SetSelected replaces the selected set.
func (f *ChecklistField) SetSelected(values []string) {
	f.selected = make(map[string]bool, len(values))
	for _, v := range values {
		f.selected[v] = true
	}
}

// Selected returns the selected options in the original option order.
func (f *ChecklistField) Selected() []string {
	var out []string
	for _, o := range f.options {
		if f.selected[o] {
			out = append(out, o)
		}
	}
	return out
}

// Label returns the field label.
func (f *ChecklistField) Label() string { return f.label }

// Focus / Blur / IsFocused — focus state.
func (f *ChecklistField) Focus()          { f.focused = true }
func (f *ChecklistField) Blur()           { f.focused = false; f.filterActive = false }
func (f *ChecklistField) IsFocused() bool { return f.focused }

// visible returns the options that match the current filter, in original
// order. Empty filter returns all options.
func (f *ChecklistField) visible() []string {
	if f.filter == "" {
		return f.options
	}
	q := strings.ToLower(f.filter)
	out := make([]string, 0, len(f.options))
	for _, o := range f.options {
		if strings.Contains(strings.ToLower(o), q) {
			out = append(out, o)
		}
	}
	return out
}

// clampCursor keeps the cursor within the visible range and scrolls the
// viewport if needed. Call after any change to the filter or cursor.
func (f *ChecklistField) clampCursor() {
	v := len(f.visible())
	if v == 0 {
		f.cursor = 0
		f.viewportStart = 0
		return
	}
	if f.cursor >= v {
		f.cursor = v - 1
	}
	if f.cursor < 0 {
		f.cursor = 0
	}
	f.scrollIntoView()
}

// scrollIntoView adjusts viewportStart so the cursor sits inside the
// visible window.
func (f *ChecklistField) scrollIntoView() {
	if f.cursor < f.viewportStart {
		f.viewportStart = f.cursor
	}
	if f.cursor >= f.viewportStart+f.viewportSize {
		f.viewportStart = f.cursor - f.viewportSize + 1
	}
	if f.viewportStart < 0 {
		f.viewportStart = 0
	}
}

// HandleKey routes a key press to either the filter input or the list,
// per the current mode. See the type-level doc for the full key map.
func (f *ChecklistField) HandleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if len(f.options) == 0 {
		return false, nil
	}
	if f.filterActive {
		return f.handleFilterKey(msg)
	}
	return f.handleListKey(msg)
}

func (f *ChecklistField) handleFilterKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	s := msg.String()
	switch s {
	case "enter", "esc":
		f.filterActive = false
		return true, nil
	case "up", "down":
		f.filterActive = false
		return f.handleListKey(msg)
	case "backspace":
		if f.filter != "" {
			f.filter = f.filter[:len(f.filter)-1]
			f.clampCursor()
			return true, nil
		}
		f.filterActive = false
		return true, nil
	case "space":
		return f.toggleUnderCursor()
	case "tab", "shift+tab":
		f.filterActive = false
		return false, nil
	}
	// Printable ASCII extends the filter string.
	if len(s) == 1 {
		c := s[0]
		if c >= 0x20 && c < 0x7f {
			f.filter += s
			f.clampCursor()
			return true, nil
		}
	}
	return true, nil
}

func (f *ChecklistField) handleListKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	visible := f.visible()
	switch msg.String() {
	case "up":
		if f.cursor > 0 {
			f.cursor--
			f.scrollIntoView()
			return true, nil
		}
		return false, nil

	case "down":
		if f.cursor < len(visible)-1 {
			f.cursor++
			f.scrollIntoView()
			return true, nil
		}
		return false, nil

	case "home":
		f.cursor = 0
		f.scrollIntoView()
		return true, nil

	case "end":
		if n := len(visible); n > 0 {
			f.cursor = n - 1
			f.scrollIntoView()
		}
		return true, nil

	case "pgup":
		f.cursor -= f.viewportSize
		if f.cursor < 0 {
			f.cursor = 0
		}
		f.scrollIntoView()
		return true, nil

	case "pgdown":
		f.cursor += f.viewportSize
		if n := len(visible); f.cursor >= n {
			f.cursor = n - 1
		}
		f.scrollIntoView()
		return true, nil

	case "space":
		return f.toggleUnderCursor()

	case "/":
		f.filterActive = true
		return true, nil

	case "a":
		for _, o := range visible {
			f.selected[o] = true
		}
		return true, nil

	case "n":
		for _, o := range visible {
			delete(f.selected, o)
		}
		return true, nil

	case "i":
		for _, o := range visible {
			f.selected[o] = !f.selected[o]
		}
		return true, nil

	case "esc":
		if f.filter != "" {
			f.filter = ""
			f.clampCursor()
			return true, nil
		}
		return false, nil
	}
	return false, nil
}

func (f *ChecklistField) toggleUnderCursor() (bool, tea.Cmd) {
	visible := f.visible()
	if f.cursor >= 0 && f.cursor < len(visible) {
		name := visible[f.cursor]
		f.selected[name] = !f.selected[name]
	}
	return true, nil
}

// summary renders the one-line description of the current selection.
func (f *ChecklistField) summary(styles ui.Styles) string {
	if len(f.options) == 0 {
		return styles.Help.Render(f.emptyHint)
	}
	sel := f.Selected()
	if len(sel) == 0 {
		return styles.Help.Render(f.allHint)
	}
	n := len(sel)
	switch {
	case n == 1:
		return sel[0]
	case n == 2:
		return sel[0] + ", " + sel[1]
	case n <= 3:
		return strings.Join(sel, ", ")
	default:
		return fmt.Sprintf("%d selected  (%s, %s, +%d)", n, sel[0], sel[1], n-2)
	}
}

// View renders the blurred summary, or (when focused) the summary plus a
// bordered inner box with filter input, viewport, and hint line.
func (f *ChecklistField) View(_ int, styles ui.Styles) string {
	label := padLabel(f.label, LabelWidth)
	if f.focused {
		label = styles.Accent.Render(label)
	} else {
		label = styles.Normal.Render(label)
	}

	// Summary with [N/M · K match] counter when focused and relevant.
	summary := f.summary(styles)
	if f.focused && len(f.options) > 0 {
		total := len(f.options)
		selCount := len(f.Selected())
		counter := fmt.Sprintf("  [%d/%d", selCount, total)
		if f.filter != "" {
			counter += fmt.Sprintf(" · %d match", len(f.visible()))
		}
		counter += "]"
		summary += styles.Help.Render(counter)
	}

	head := label + summary
	if !f.focused || len(f.options) == 0 {
		return head
	}

	// Focused: draw the inner picker box.
	return head + "\n" + f.viewPicker(styles)
}

// viewPicker renders the bordered filter + viewport + hint block.
func (f *ChecklistField) viewPicker(styles ui.Styles) string {
	visible := f.visible()

	// Filter line
	var filterPrompt string
	if f.filterActive {
		filterPrompt = styles.Accent.Render("/ ")
	} else {
		filterPrompt = styles.Help.Render("/ ")
	}
	filterText := f.filter
	if f.filterActive {
		filterText += "_"
	} else if filterText == "" {
		filterText = styles.Help.Render("(type / to filter)")
	}
	filterLine := filterPrompt + filterText

	// Viewport rows
	var rows []string
	start := f.viewportStart
	end := start + f.viewportSize
	if end > len(visible) {
		end = len(visible)
	}
	if start > 0 {
		rows = append(rows, styles.Help.Render(fmt.Sprintf("  … %d more above", start)))
	}
	for i := start; i < end; i++ {
		opt := visible[i]
		check := "[ ]"
		if f.selected[opt] {
			check = "[x]"
		}
		prefix := "  "
		if i == f.cursor {
			prefix = styles.Accent.Render("▸ ")
		}
		row := prefix + check + " " + opt
		if i == f.cursor {
			row = styles.Selected.Render(row)
		}
		rows = append(rows, row)
	}
	if end < len(visible) {
		rows = append(rows, styles.Help.Render(fmt.Sprintf("  … %d more below", len(visible)-end)))
	}

	// Empty filter result
	if len(visible) == 0 {
		rows = []string{styles.Help.Render("  (no matches)")}
	}

	// Hint
	hint := styles.Help.Render("space toggle · / filter · a all · n none · i invert · esc clear")

	body := strings.Join(append(append([]string{filterLine}, rows...), hint), "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(styles.Theme.Overlay).
		Padding(0, 1).
		Render(body)
	return box
}
