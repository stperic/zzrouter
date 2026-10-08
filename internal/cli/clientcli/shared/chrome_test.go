package shared

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/pkg/ui"
)

func testStyles() ui.Styles { return ui.NewStyles(ui.CatppuccinMocha()) }

// widestLine reports the display width of the widest line in s, which is what
// a terminal clips against.
func widestLine(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if lw := lipgloss.Width(line); lw > w {
			w = lw
		}
	}
	return w
}

func TestWrapTextFitsWidth(t *testing.T) {
	in := strings.Repeat("token ", 60)
	got := WrapText(in, 40)
	if w := widestLine(got); w > 40 {
		t.Fatalf("widest line = %d, want <= 40", w)
	}
	if !strings.Contains(got, "\n") {
		t.Fatal("expected the input to be split across lines")
	}
}

func TestWrapTextBreaksUnbreakableWords(t *testing.T) {
	got := WrapText(strings.Repeat("x", 200), 30)
	if w := widestLine(got); w > 30 {
		t.Fatalf("widest line = %d, want <= 30", w)
	}
}

func TestWrapTextPreservesStyling(t *testing.T) {
	styled := lipgloss.NewStyle().Bold(true).Render(strings.Repeat("word ", 30))
	got := WrapText(styled, 20)
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "\x1b[") {
			t.Fatalf("wrapped line lost its styling: %q", line)
		}
		if w := lipgloss.Width(line); w > 20 {
			t.Fatalf("styled line width = %d, want <= 20", w)
		}
	}
}

func TestWrapTextPassesThroughBelowMinWidth(t *testing.T) {
	in := strings.Repeat("a b ", 20)
	// Views render at width 0 before their first WindowSizeMsg.
	if got := WrapText(in, 0); got != in {
		t.Fatal("width 0 must return the input untouched")
	}
	if got := WrapText(in, MinWrapWidth-1); got != in {
		t.Fatalf("width %d must return the input untouched", MinWrapWidth-1)
	}
}

func TestWrapIndentIndentsEveryLine(t *testing.T) {
	got := WrapIndent(strings.Repeat("word ", 40), "  ", 30)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatal("expected multiple lines")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("line missing indent: %q", line)
		}
		if w := lipgloss.Width(line); w > 30 {
			t.Fatalf("line width = %d (indent included), want <= 30", w)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"fits", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"cuts", "hello world", 8, "hello..."},
		{"tiny budget", "hello", 2, "he"},
		{"zero budget", "hello", 0, ""},
		{"multibyte stays intact", "héllo wörld ünïcödé", 10, "héllo w..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncateRunes(tt.in, tt.n); got != tt.want {
				t.Fatalf("TruncateRunes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestDetailFieldWrapsLongValue(t *testing.T) {
	const width = 60
	d := NewDetail(testStyles(), width)
	d.Field("Allowed Models", strings.Repeat("some-model-name, ", 12))

	lines := strings.Split(strings.TrimRight(d.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the value to wrap, got %d line(s)", len(lines))
	}
	for _, line := range lines {
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("line width = %d, want <= %d: %q", w, width, line)
		}
	}
	// Continuation lines align under the value column, not at the left edge.
	col := len("Allowed Models:") + 2
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, strings.Repeat(" ", col)) {
			t.Fatalf("continuation line not aligned to the value column: %q", line)
		}
	}
}

func TestDetailKeyColumnSizesPerSection(t *testing.T) {
	const width = 80
	d := NewDetail(testStyles(), width)
	d.Section("Configuration")
	d.Field("ID", "team_eng")
	d.Field("Allowed Models", "qwen3-27b")
	d.Section("Keys")
	d.Field("vk_8f2a1c9e (Eric's laptop)", "owner")
	d.Field("vk_3d7b0f14", "member")

	got := valueColumns(t, d.String())
	// Section 1 sizes to "Allowed Models:", section 2 to the long key label.
	want := map[string]int{
		"team_eng":  len(" Allowed Models:") + 1,
		"qwen3-27b": len(" Allowed Models:") + 1,
		"owner":     len(" vk_8f2a1c9e (Eric's laptop):") + 1,
		"member":    len(" vk_8f2a1c9e (Eric's laptop):") + 1,
	}
	for val, wantCol := range want {
		if got[val] != wantCol {
			t.Errorf("value %q starts at column %d, want %d", val, got[val], wantCol)
		}
	}
}

func TestDetailKeyColumnCapped(t *testing.T) {
	const width = 60
	d := NewDetail(testStyles(), width)
	d.Field(strings.Repeat("k", 200), "value")
	d.Field("ID", "team_eng")

	// The pathological key overflows its own row, but must not drag the
	// column for the rest of the section past the cap.
	col := valueColumns(t, d.String())["team_eng"]
	if maxCol := 1 + width/MaxDetailKeyDivisor + 1; col > maxCol {
		t.Fatalf("value column at %d, want <= %d", col, maxCol)
	}
}

// valueColumns maps each field value to the column its first cell sits in.
func valueColumns(t *testing.T, out string) map[string]int {
	t.Helper()
	cols := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		plain := stripANSI(line)
		for _, val := range []string{"team_eng", "qwen3-27b", "owner", "member", "value"} {
			if idx := strings.Index(plain, " "+val); idx >= 0 && strings.HasSuffix(strings.TrimRight(plain, " "), val) {
				cols[val] = idx + 1
			}
		}
	}
	return cols
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestDetailFieldShortValueStaysOnOneLine(t *testing.T) {
	d := NewDetail(testStyles(), 60)
	d.Field("Node", "worker-1")
	if got := strings.Count(strings.TrimRight(d.String(), "\n"), "\n"); got != 0 {
		t.Fatalf("short value split across %d extra line(s)", got)
	}
}

func TestDetailFieldUnwrappedAtZeroWidth(t *testing.T) {
	long := strings.Repeat("value ", 40)
	d := NewDetail(testStyles(), 0)
	d.Field("Key", long)
	if got := strings.Count(strings.TrimRight(d.String(), "\n"), "\n"); got != 0 {
		t.Fatal("zero width must not wrap — there is no budget to wrap against")
	}
}

func TestDetailTextWrapsAndIndents(t *testing.T) {
	const width = 50
	d := NewDetail(testStyles(), width)
	d.Text(" ", "You are a helpful assistant. "+strings.Repeat("Answer concisely. ", 10))

	lines := strings.Split(strings.TrimRight(d.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatal("expected the prompt to wrap")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, " ") {
			t.Fatalf("line missing indent: %q", line)
		}
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("line width = %d, want <= %d", w, width)
		}
	}
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

func TestMeasureColumnsSizesToContent(t *testing.T) {
	got := MeasureColumns(ColumnLayout{
		Headers: []string{"KIND", "TARGET", "BYTES"},
		Rows: [][]string{
			{"download", "Qwen3.8-27B-Instruct-Q8_0-GGUF", "12.9 GB"},
			{"install", "mlx", "100%"},
		},
		Width: 120,
		Pad:   1,
		Flex:  1,
	})
	// Widest cell + pad, not a fixed constant.
	if want := len("download") + 1; got[0] != want {
		t.Errorf("KIND width = %d, want %d", got[0], want)
	}
	if want := len("Qwen3.8-27B-Instruct-Q8_0-GGUF") + 1; got[1] != want {
		t.Errorf("TARGET width = %d, want %d", got[1], want)
	}
	// The last column absorbs the leftover width.
	if sum(got) != 120 {
		t.Errorf("columns total %d, want the full 120", sum(got))
	}
}

func TestMeasureColumnsShrinksFlexToFit(t *testing.T) {
	const width = 50
	got := MeasureColumns(ColumnLayout{
		Headers: []string{"KIND", "TARGET", "NODE", "BYTES"},
		Rows: [][]string{
			{"download", strings.Repeat("m", 120), "worker-1", "12.9 GB / 30.1 GB"},
		},
		Width: width,
		Pad:   1,
		Flex:  1,
	})
	if sum(got) > width {
		t.Fatalf("columns total %d, want <= %d", sum(got), width)
	}
	if got[1] < MinFlexColumnWidth {
		t.Fatalf("flex column shrank to %d, below the %d floor", got[1], MinFlexColumnWidth)
	}
	// Only the flex column gives way; the others keep their content width.
	if want := len("worker-1") + 1; got[2] != want {
		t.Errorf("NODE width = %d, want %d — non-flex columns must not shrink", got[2], want)
	}
}

func TestMeasureColumnsRespectsMins(t *testing.T) {
	got := MeasureColumns(ColumnLayout{
		Headers: []string{"A", "B", "C"},
		Rows:    [][]string{{"x", "y", "z"}},
		Mins:    []int{30, 0, 0},
		Width:   120,
		Pad:     1,
		Flex:    1,
	})
	if got[0] != 30 {
		t.Fatalf("column A = %d, want its 30-cell minimum", got[0])
	}
}

func TestMeasureColumnsZeroWidthKeepsContentWidths(t *testing.T) {
	got := MeasureColumns(ColumnLayout{
		Headers: []string{"A", "B", "C"},
		Rows:    [][]string{{"alpha", "b", "c"}},
		Pad:     1,
		Flex:    1,
	})
	if want := len("alpha") + 1; got[0] != want {
		t.Fatalf("column A = %d, want %d", got[0], want)
	}
}
