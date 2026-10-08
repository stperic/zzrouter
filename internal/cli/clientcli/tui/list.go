package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// NavigationKeys defines the keymap used by any scrollable primitive
// (List, Detail, and future viewports). Views can override bindings to
// match their existing UX by assigning before passing to the primitive.
type NavigationKeys struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
}

// DefaultNavigationKeys is the conventional set used across the zzrouter TUI.
func DefaultNavigationKeys() NavigationKeys {
	return NavigationKeys{
		Up:       key.NewBinding(key.WithKeys("up", "k")),
		Down:     key.NewBinding(key.WithKeys("down", "j")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		Home:     key.NewBinding(key.WithKeys("home")),
		End:      key.NewBinding(key.WithKeys("end")),
	}
}

// List holds cursor/offset state for a scrollable list of Items. It is a
// plain value struct — no allocations, safe to embed in view models. Views
// call UpdateKey for key events, UpdateWheel for wheel events, and read
// Cursor/Offset/Visible when rendering.
type List struct {
	items   []Item
	cursor  int
	offset  int
	visible int // computed each frame from height budget
	keys    NavigationKeys
}

// NewList creates a List with default keys and no items.
func NewList() List {
	return List{keys: DefaultNavigationKeys()}
}

// SetKeys overrides the keymap in place.
func (l *List) SetKeys(k NavigationKeys) { l.keys = k }

// SetItems replaces the items slice, clamps the cursor, and advances past
// any leading Separator so the initial cursor sits on a selectable item.
func (l *List) SetItems(items []Item) {
	l.items = items
	if l.cursor >= len(items) {
		l.cursor = len(items) - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	l.cursor = l.nearestSelectable(l.cursor, +1)
	l.clampOffset()
}

// SetVisible records how many rows fit. Call each render before reading Offset.
func (l *List) SetVisible(n int) {
	if n < 1 {
		n = 1
	}
	l.visible = n
	l.clampOffset()
}

// Items returns the backing slice. Callers MUST treat it as read-only; the
// List clamps cursor/offset against len(items) and will misbehave if the
// caller mutates the slice out from under it. Use SetItems to replace.
func (l List) Items() []Item { return l.items }

// Cursor returns the selected index. -1 if the list is empty.
func (l List) Cursor() int {
	if len(l.items) == 0 {
		return -1
	}
	return l.cursor
}

// SetCursor moves the cursor, clamping to bounds, skipping separators,
// and advancing Offset as needed. If no selectable row exists the cursor
// stays at its clamped index (caller-visible via Selected() returning nil
// when the item happens to be a Separator).
func (l *List) SetCursor(i int) {
	if len(l.items) == 0 {
		l.cursor, l.offset = 0, 0
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= len(l.items) {
		i = len(l.items) - 1
	}
	l.cursor = l.nearestSelectable(i, +1)
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.visible > 0 && l.cursor >= l.offset+l.visible {
		l.offset = l.cursor - l.visible + 1
	}
	l.clampOffset()
}

// Offset returns the first visible row index.
func (l List) Offset() int { return l.offset }

// Selected returns the item under the cursor, or nil if empty.
func (l List) Selected() Item {
	c := l.Cursor()
	if c < 0 {
		return nil
	}
	return l.items[c]
}

// Len returns the item count.
func (l List) Len() int { return len(l.items) }

// UpdateKey handles navigation keys. Returns true if the key matched.
func (l *List) UpdateKey(msg tea.KeyPressMsg) bool {
	switch {
	case key.Matches(msg, l.keys.Up):
		l.moveCursor(-1)
	case key.Matches(msg, l.keys.Down):
		l.moveCursor(+1)
	case key.Matches(msg, l.keys.PageUp):
		l.page(-1)
	case key.Matches(msg, l.keys.PageDown):
		l.page(+1)
	case key.Matches(msg, l.keys.Home):
		l.SetCursor(0)
	case key.Matches(msg, l.keys.End):
		l.SetCursor(len(l.items) - 1)
	default:
		return false
	}
	return true
}

// UpdateWheel handles mouse wheel events. When all rows fit on screen, the
// wheel moves the cursor; otherwise it scrolls offset.
func (l *List) UpdateWheel(msg tea.MouseWheelMsg) {
	if len(l.items) == 0 {
		return
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		if l.offset > 0 {
			l.offset--
			if l.visible > 0 && l.cursor >= l.offset+l.visible {
				l.cursor = l.offset + l.visible - 1
			}
		} else if l.cursor > 0 {
			l.cursor--
		}
	case tea.MouseWheelDown:
		maxOff := l.maxOffset()
		if l.offset < maxOff {
			l.offset++
			if l.cursor < l.offset {
				l.cursor = l.offset
			}
		} else if l.cursor < len(l.items)-1 {
			l.cursor++
		}
	}
}

// Click maps a mouse click Y coordinate to a row and updates the cursor.
// chromeLines is the number of lines above the first data row (typically
// title bar + breadcrumb + blank + table header + separator). Callers pass
// a package-level constant that matches their view's chrome height.
//
// Returns (enter, ok). ok=false means the click missed the table body
// (caller should ignore). ok=true means a row was clicked: enter=true when
// the cursor was already on that row (double-click-to-enter convention),
// enter=false when the cursor moved to a new row.
//
// Replaces per-view duplication of the "Y - chromeLines + offset, bounds
// check, equal-cursor-means-enter" boilerplate that existed in every list
// view's handleMouseClick.
func (l *List) Click(msg tea.MouseClickMsg, chromeLines int) (enter, ok bool) {
	row := msg.Y - chromeLines + l.offset
	if row < 0 || row >= len(l.items) {
		return false, false
	}
	enter = l.cursor == row
	l.SetCursor(row)
	return enter, true
}

// VisibleRange returns [first, last) row indices currently visible. Callers
// iterate the returned range when rendering.
func (l List) VisibleRange() (int, int) {
	if len(l.items) == 0 {
		return 0, 0
	}
	last := l.offset + l.visible
	if last > len(l.items) {
		last = len(l.items)
	}
	if last < l.offset {
		last = l.offset
	}
	return l.offset, last
}

// IsCursor reports whether row i is the highlighted row.
func (l List) IsCursor(i int) bool { return i == l.cursor }

// --- internal ---

func (l *List) moveCursor(delta int) {
	if len(l.items) == 0 {
		return
	}
	i := l.cursor + delta
	for i >= 0 && i < len(l.items) && !IsSelectable(l.items[i]) {
		i += delta
	}
	if i < 0 || i >= len(l.items) {
		return
	}
	l.SetCursor(i)
}

func (l *List) page(dir int) {
	if l.visible <= 0 {
		return
	}
	l.offset += dir * l.visible
	l.clampOffset()
	l.cursor = l.nearestSelectable(l.offset, dir)
}

// nearestSelectable returns the nearest index >= start that is a selectable
// row. preferDir is the direction to search first (+1 = forward, -1 = back).
// If no selectable item is found in the preferred direction, it searches
// the other direction. Returns start if nothing is selectable anywhere.
func (l List) nearestSelectable(start, preferDir int) int {
	if len(l.items) == 0 {
		return 0
	}
	if start < 0 {
		start = 0
	}
	if start >= len(l.items) {
		start = len(l.items) - 1
	}
	if IsSelectable(l.items[start]) {
		return start
	}
	// Search preferred direction first.
	for i := start + preferDir; i >= 0 && i < len(l.items); i += preferDir {
		if IsSelectable(l.items[i]) {
			return i
		}
	}
	// Fall back to the other direction.
	other := -preferDir
	for i := start + other; i >= 0 && i < len(l.items); i += other {
		if IsSelectable(l.items[i]) {
			return i
		}
	}
	return start
}

func (l *List) clampOffset() {
	maxOff := l.maxOffset()
	if l.offset < 0 {
		l.offset = 0
	}
	if l.offset > maxOff {
		l.offset = maxOff
	}
}

func (l List) maxOffset() int {
	if l.visible <= 0 || len(l.items) <= l.visible {
		return 0
	}
	return len(l.items) - l.visible
}
