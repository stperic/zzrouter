package tui

import "testing"

func TestRenderedHeight(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\nb\nc", 3},
	}
	for _, c := range cases {
		got := RenderedHeight(c.in)
		if got != c.want {
			t.Errorf("RenderedHeight(%q) = %d want %d", c.in, got, c.want)
		}
	}
}

func TestVisibleRowsFloor(t *testing.T) {
	if got := VisibleRows(0, ""); got != 3 {
		t.Errorf("floor want 3 got %d", got)
	}
}

func TestVisibleRowsSubtractsChrome(t *testing.T) {
	// height 10, footer = 1 row → 10 - 2 (header) - 1 (terminator) - 1 (footer) = 6
	if got := VisibleRows(10, "hint"); got != 6 {
		t.Errorf("want 6 got %d", got)
	}
}

func TestTruncateFits(t *testing.T) {
	if got := Truncate("hello", 10, "…"); got != "hello" {
		t.Errorf("no-truncate want 'hello' got %q", got)
	}
}

func TestTruncateClamps(t *testing.T) {
	got := Truncate("hello world", 8, "…")
	if got != "hello w…" {
		t.Errorf("want 'hello w…' got %q", got)
	}
}

func TestTruncatePreservesAnsi(t *testing.T) {
	// Red "hello" + space + green "world" — width=11 ignoring ANSI.
	in := "\x1b[31mhello\x1b[0m \x1b[32mworld\x1b[0m"
	// Fits: no change.
	if got := Truncate(in, 20, "…"); got != in {
		t.Errorf("within width should not truncate: got %q", got)
	}
	// Display width should be measured ignoring ANSI, and ANSI prefix must
	// be retained when truncating.
	got := Truncate(in, 5, "…")
	// Visible width of result must be <= 5.
	if visibleWidth := visibleRunesCount(got); visibleWidth > 5 {
		t.Errorf("visible width %d exceeds 5 for %q", visibleWidth, got)
	}
}

// visibleRunesCount counts printable runes ignoring ANSI escape sequences.
func visibleRunesCount(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}
