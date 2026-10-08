package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDetailRenderClampsScroll(t *testing.T) {
	d := NewDetail()
	content := strings.Join([]string{"1", "2", "3", "4", "5"}, "\n")

	// Request scroll beyond end.
	end := tea.KeyPressMsg{Code: tea.KeyEnd, Text: "end"}
	d.UpdateKey(end)
	out := d.Render(content, 3)
	if out != "3\n4\n5" {
		t.Fatalf("want tail of 3 lines, got %q", out)
	}
}

func TestDetailPageDown(t *testing.T) {
	d := NewDetail()
	content := strings.Join([]string{"1", "2", "3", "4", "5", "6"}, "\n")
	// First Render sets visible=2; then PgDown scrolls by 2.
	_ = d.Render(content, 2)
	pg := tea.KeyPressMsg{Code: tea.KeyPgDown, Text: "pgdown"}
	d.UpdateKey(pg)
	if d.Scroll() != 2 {
		t.Fatalf("scroll want 2 got %d", d.Scroll())
	}
}

func TestDetailWheelScrolls(t *testing.T) {
	d := NewDetail()
	d.UpdateWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if d.Scroll() != 1 {
		t.Fatalf("scroll want 1 got %d", d.Scroll())
	}
	d.UpdateWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if d.Scroll() != 0 {
		t.Fatalf("scroll want 0 got %d", d.Scroll())
	}
}
