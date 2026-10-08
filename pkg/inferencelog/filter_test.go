package inferencelog

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileLogFilter_Nil(t *testing.T) {
	f, err := CompileLogFilter("", "", 0, 0)
	if err != nil {
		t.Fatalf("CompileLogFilter: %v", err)
	}
	if f != nil {
		t.Errorf("want nil filter, got %+v", f)
	}
}

func TestCompileLogFilter_MutualExclusive(t *testing.T) {
	_, err := CompileLogFilter("error", "er.*", 0, 0)
	if err == nil {
		t.Fatal("want error when both grep and regex set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompileLogFilter_InvalidRegex(t *testing.T) {
	_, err := CompileLogFilter("", "[", 0, 0)
	if err == nil {
		t.Fatal("want error on invalid regex")
	}
}

func TestCompileLogFilter_ContextCap(t *testing.T) {
	_, err := CompileLogFilter("x", "", LogFilterMaxContext+1, 0)
	if err == nil {
		t.Fatal("want error when context exceeds max")
	}
}

func TestCompileLogFilter_MaxMatchesClamping(t *testing.T) {
	f, err := CompileLogFilter("x", "", 0, LogFilterAbsoluteMaxMatches+500)
	if err != nil {
		t.Fatalf("CompileLogFilter: %v", err)
	}
	if f.maxMatches != LogFilterAbsoluteMaxMatches {
		t.Errorf("want clamp to %d, got %d", LogFilterAbsoluteMaxMatches, f.maxMatches)
	}

	f2, _ := CompileLogFilter("x", "", 0, 0)
	if f2.maxMatches != LogFilterDefaultMaxMatches {
		t.Errorf("want default %d, got %d", LogFilterDefaultMaxMatches, f2.maxMatches)
	}
}

func TestLogFilter_MatchSubstring(t *testing.T) {
	f, _ := CompileLogFilter("error", "", 0, 0)
	cases := map[string]bool{
		"this is an ERROR message": true, // case insensitive
		"an error occurred":        true,
		"all good here":            false,
	}
	for line, want := range cases {
		if got := f.Match(line); got != want {
			t.Errorf("Match(%q)=%v, want %v", line, got, want)
		}
	}
}

func TestLogFilter_MatchRegex(t *testing.T) {
	f, err := CompileLogFilter("", `req-\d{3}`, 0, 0)
	if err != nil {
		t.Fatalf("CompileLogFilter: %v", err)
	}
	cases := map[string]bool{
		"req-123 completed": true,
		"req-99 failed":     false, // too few digits
		"no match here":     false,
	}
	for line, want := range cases {
		if got := f.Match(line); got != want {
			t.Errorf("Match(%q)=%v, want %v", line, got, want)
		}
	}
}

func TestLogFilter_NilMatchesAll(t *testing.T) {
	var f *LogFilter
	if !f.Match("anything") {
		t.Error("nil filter should match everything")
	}
}

func TestLogFilter_ApplyBasic(t *testing.T) {
	f, _ := CompileLogFilter("error", "", 0, 0)
	input := []string{
		"startup ok",
		"request 1",
		"ERROR: boom",
		"request 2",
		"error again",
		"done",
	}
	out, trunc := f.Apply(input)
	if trunc {
		t.Error("unexpected truncation")
	}
	want := []string{"ERROR: boom", "error again"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

func TestLogFilter_ApplyContextWindow(t *testing.T) {
	f, _ := CompileLogFilter("error", "", 1, 0)
	input := []string{
		"line 0",
		"line 1",
		"ERROR: boom", // index 2
		"line 3",
		"line 4",
	}
	out, _ := f.Apply(input)
	want := []string{"line 1", "ERROR: boom", "line 3"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

func TestLogFilter_ApplyContextMerging(t *testing.T) {
	// Two matches close enough that their context windows overlap.
	// Overlap should be deduplicated so each line appears once.
	f, _ := CompileLogFilter("err", "", 1, 0)
	input := []string{
		"line 0",
		"err 1", // idx 1 → window [0..2]
		"line 2",
		"err 3", // idx 3 → window [2..4]
		"line 4",
		"line 5",
	}
	out, _ := f.Apply(input)
	want := []string{"line 0", "err 1", "line 2", "err 3", "line 4"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

func TestLogFilter_ApplyMaxMatches(t *testing.T) {
	// Cap at 2 matches; the third match should not appear and the
	// response should mark truncated=true.
	f, _ := CompileLogFilter("err", "", 0, 2)
	input := []string{
		"err 1",
		"ok",
		"err 2",
		"err 3", // beyond cap
	}
	out, trunc := f.Apply(input)
	if !trunc {
		t.Error("want truncated=true")
	}
	want := []string{"err 1", "err 2"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

func TestLogFilter_ApplyEmptyInput(t *testing.T) {
	f, _ := CompileLogFilter("x", "", 0, 0)
	out, trunc := f.Apply(nil)
	if out != nil || trunc {
		t.Errorf("out=%v trunc=%v", out, trunc)
	}
}

func TestLogFilter_ApplyNoMatches(t *testing.T) {
	f, _ := CompileLogFilter("nosuch", "", 0, 0)
	input := []string{"hello", "world"}
	out, trunc := f.Apply(input)
	if len(out) != 0 || trunc {
		t.Errorf("want empty non-truncated, got %v trunc=%v", out, trunc)
	}
}

func TestLogFilter_ApplyContextNearBoundary(t *testing.T) {
	// Match at index 0 and index len-1: context windows must clamp
	// to the slice bounds without panicking.
	f, _ := CompileLogFilter("err", "", 5, 0)
	input := []string{
		"err at start",
		"mid 1",
		"mid 2",
		"err at end",
	}
	out, _ := f.Apply(input)
	want := []string{"err at start", "mid 1", "mid 2", "err at end"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

func TestLogFilter_ApplyNilPassthrough(t *testing.T) {
	var f *LogFilter
	input := []string{"a", "b"}
	out, _ := f.Apply(input)
	if !reflect.DeepEqual(out, input) {
		t.Errorf("nil filter should passthrough: got %v", out)
	}
}

// TestLogFilter_ApplyMaxMatchesExactlyEqual verifies the edge where
// the number of hits in the input is exactly equal to max_matches:
// every match should be returned with truncated=false, since the cap
// was met but not exceeded.
func TestLogFilter_ApplyMaxMatchesExactlyEqual(t *testing.T) {
	f, _ := CompileLogFilter("err", "", 0, 3)
	input := []string{
		"err 1",
		"ok",
		"err 2",
		"ok",
		"err 3",
	}
	out, trunc := f.Apply(input)
	if trunc {
		t.Errorf("want truncated=false when hits equal cap, got true")
	}
	want := []string{"err 1", "err 2", "err 3"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

// TestLogFilter_ApplyRegexContextMerging covers the context-window
// merge path via the regex matcher (existing coverage only exercises
// the substring matcher). Two overlapping windows should collapse to
// one, preserving order with no duplicates.
func TestLogFilter_ApplyRegexContextMerging(t *testing.T) {
	f, err := CompileLogFilter("", `req-\d+`, 1, 0)
	if err != nil {
		t.Fatalf("CompileLogFilter: %v", err)
	}
	input := []string{
		"line 0",
		"req-1 start", // idx 1 → window [0..2]
		"line 2",
		"req-2 done", // idx 3 → window [2..4]
		"line 4",
		"line 5",
	}
	out, trunc := f.Apply(input)
	if trunc {
		t.Errorf("unexpected truncation")
	}
	want := []string{"line 0", "req-1 start", "line 2", "req-2 done", "line 4"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out=%v, want %v", out, want)
	}
}

// TestLogFilter_GrepPreservesLiterals is a regression check for the
// (?i)+QuoteMeta compile path: regex metacharacters in a grep query
// must match literally, not get interpreted.
func TestLogFilter_GrepPreservesLiterals(t *testing.T) {
	f, err := CompileLogFilter("a.c", "", 0, 0)
	if err != nil {
		t.Fatalf("CompileLogFilter: %v", err)
	}
	if !f.Match("A.C suffix") {
		t.Error("want literal match on 'A.C'")
	}
	if f.Match("abc") {
		t.Error("dot must not be treated as regex metacharacter")
	}
}
