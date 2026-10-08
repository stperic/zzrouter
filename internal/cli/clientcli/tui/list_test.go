package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

type testItem struct {
	title string
	desc  string
	sep   bool
}

func (t testItem) Title() string       { return t.title }
func (t testItem) Description() string { return t.desc }
func (t testItem) FilterValue() string { return t.title }
func (t testItem) Separator() bool     { return t.sep }

func items(titles ...string) []Item {
	out := make([]Item, len(titles))
	for i, tt := range titles {
		out[i] = testItem{title: tt}
	}
	return out
}

func TestListCursorClampsOnSetItems(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c", "d"))
	l.SetCursor(3)
	l.SetItems(items("x", "y"))
	if l.Cursor() != 1 {
		t.Fatalf("cursor want 1, got %d", l.Cursor())
	}
}

func TestListCursorEmpty(t *testing.T) {
	l := NewList()
	if l.Cursor() != -1 {
		t.Fatalf("empty list cursor want -1, got %d", l.Cursor())
	}
	if l.Selected() != nil {
		t.Fatalf("empty list selected want nil")
	}
}

func TestListUpDownKeys(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c"))
	l.SetVisible(10)

	down := tea.KeyPressMsg{Code: 'j', Text: "j"}
	if !l.UpdateKey(down) {
		t.Fatal("expected down to match")
	}
	if l.Cursor() != 1 {
		t.Fatalf("cursor want 1 got %d", l.Cursor())
	}
	up := tea.KeyPressMsg{Code: 'k', Text: "k"}
	if !l.UpdateKey(up) {
		t.Fatal("expected up to match")
	}
	if l.Cursor() != 0 {
		t.Fatalf("cursor want 0 got %d", l.Cursor())
	}
}

func TestListWheelScrollsOffsetBeforeCursor(t *testing.T) {
	l := NewList()
	l.SetItems(items("1", "2", "3", "4", "5", "6", "7", "8"))
	l.SetVisible(3)

	// Scroll down 3 times: offset should advance while cursor stays visible.
	wheel := tea.MouseWheelMsg{Button: tea.MouseWheelDown}
	l.UpdateWheel(wheel)
	l.UpdateWheel(wheel)
	l.UpdateWheel(wheel)
	if l.Offset() != 3 {
		t.Fatalf("offset want 3, got %d", l.Offset())
	}
}

func TestListSeparatorIsSkipped(t *testing.T) {
	l := NewList()
	l.SetItems([]Item{
		testItem{title: "a"},
		testItem{title: "---", sep: true},
		testItem{title: "b"},
	})
	l.SetVisible(10)
	l.SetCursor(0)

	down := tea.KeyPressMsg{Code: 'j', Text: "j"}
	l.UpdateKey(down)
	if l.Cursor() != 2 {
		t.Fatalf("cursor should skip separator; want 2 got %d", l.Cursor())
	}
}

func TestListPageDown(t *testing.T) {
	l := NewList()
	l.SetItems(items("1", "2", "3", "4", "5", "6"))
	l.SetVisible(2)
	l.SetCursor(0)

	pgdn := tea.KeyPressMsg{Code: tea.KeyPgDown, Text: "pgdown"}
	l.UpdateKey(pgdn)
	if l.Offset() != 2 {
		t.Fatalf("offset want 2 got %d", l.Offset())
	}
	if l.Cursor() != 2 {
		t.Fatalf("cursor want 2 got %d", l.Cursor())
	}
}

func TestListInitialCursorSkipsLeadingSeparator(t *testing.T) {
	l := NewList()
	l.SetItems([]Item{
		testItem{title: "---", sep: true},
		testItem{title: "a"},
	})
	if l.Cursor() != 1 {
		t.Fatalf("initial cursor should land on first selectable; want 1 got %d", l.Cursor())
	}
	if l.Selected() == nil || l.Selected().Title() != "a" {
		t.Fatalf("selected should be 'a'")
	}
}

func TestListPageDownSkipsTrailingSeparators(t *testing.T) {
	l := NewList()
	l.SetItems([]Item{
		testItem{title: "1"},
		testItem{title: "2"},
		testItem{title: "---", sep: true},
		testItem{title: "3"},
	})
	l.SetVisible(2)
	l.SetCursor(0)

	pgdn := tea.KeyPressMsg{Code: tea.KeyPgDown, Text: "pgdown"}
	l.UpdateKey(pgdn)
	// Landing at offset=2 would land on the separator; nearestSelectable
	// advances to index 3.
	if l.Cursor() != 3 {
		t.Fatalf("PgDn should skip separator at landing; cursor want 3 got %d", l.Cursor())
	}
}

func TestListVisibleRange(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c", "d", "e"))
	l.SetVisible(2)
	l.SetCursor(3) // forces offset = 2

	start, end := l.VisibleRange()
	if start != 2 || end != 4 {
		t.Fatalf("visible range want [2,4) got [%d,%d)", start, end)
	}
}

func TestListClickMissesOutsideTable(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c"))
	// Click above the table body (chromeLines=6, Y=2 puts row negative).
	_, ok := l.Click(tea.MouseClickMsg{Y: 2}, 6)
	if ok {
		t.Fatalf("click above table should miss")
	}
	// Click past the last row.
	_, ok = l.Click(tea.MouseClickMsg{Y: 20}, 6)
	if ok {
		t.Fatalf("click past last row should miss")
	}
}

func TestListClickMovesCursor(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c"))
	// Y=7 with chromeLines=6 → row 1.
	enter, ok := l.Click(tea.MouseClickMsg{Y: 7}, 6)
	if !ok {
		t.Fatalf("click should land on row 1")
	}
	if enter {
		t.Fatalf("first click (cursor was 0, row is 1) should not signal enter")
	}
	if l.Cursor() != 1 {
		t.Fatalf("cursor want 1, got %d", l.Cursor())
	}
}

func TestListClickDoubleClickEnters(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c"))
	l.SetCursor(2)
	// Click on the cursor row — should signal enter.
	enter, ok := l.Click(tea.MouseClickMsg{Y: 8}, 6) // Y=8 → row 2
	if !ok {
		t.Fatalf("click should land")
	}
	if !enter {
		t.Fatalf("click on already-selected row should signal enter")
	}
}

func TestListClickHonorsOffset(t *testing.T) {
	l := NewList()
	l.SetItems(items("a", "b", "c", "d", "e"))
	l.SetVisible(2)
	l.SetCursor(3) // forces offset = 2
	// Y=6 with chromeLines=6 and offset=2 → row 2 (item "c").
	_, ok := l.Click(tea.MouseClickMsg{Y: 6}, 6)
	if !ok {
		t.Fatalf("click at top-visible row should land")
	}
	if l.Cursor() != 2 {
		t.Fatalf("cursor want 2, got %d", l.Cursor())
	}
}
