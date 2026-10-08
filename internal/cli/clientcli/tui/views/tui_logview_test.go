package views

import (
	"reflect"
	"testing"
)

func TestLogLineRing_AppendUnderCap(t *testing.T) {
	r := newLogLineRing(4)
	for _, s := range []string{"a", "b", "c"} {
		if r.Append(s) {
			t.Errorf("unexpected eviction for %q", s)
		}
	}
	if r.Len() != 3 {
		t.Fatalf("len=%d, want 3", r.Len())
	}
	if got := r.Snapshot(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("snapshot=%v", got)
	}
}

func TestLogLineRing_AppendEvicts(t *testing.T) {
	r := newLogLineRing(3)
	for _, s := range []string{"a", "b", "c"} {
		if r.Append(s) {
			t.Errorf("unexpected eviction for %q", s)
		}
	}
	if !r.Append("d") {
		t.Error("want eviction when appending past capacity")
	}
	if !r.Append("e") {
		t.Error("want eviction on subsequent append")
	}
	if got := r.Snapshot(); !reflect.DeepEqual(got, []string{"c", "d", "e"}) {
		t.Errorf("snapshot=%v", got)
	}
	// At() must reflect logical indexing.
	if r.At(0) != "c" || r.At(2) != "e" {
		t.Errorf("At: %q %q", r.At(0), r.At(2))
	}
}

func TestLogLineRing_Reset(t *testing.T) {
	r := newLogLineRing(3)
	r.Append("a")
	r.Append("b")
	r.Reset()
	if r.Len() != 0 {
		t.Fatalf("len=%d, want 0", r.Len())
	}
	r.Append("x")
	if r.At(0) != "x" {
		t.Errorf("At(0)=%q, want x", r.At(0))
	}
}

func TestFollowState_Badge(t *testing.T) {
	cases := map[followState]string{
		followLive:    "● LIVE",
		followPaused:  "⏸ PAUSED",
		followStopped: "⏹ STOPPED",
	}
	for s, want := range cases {
		if got := s.Badge(); got != want {
			t.Errorf("%v badge=%q, want %q", s, got, want)
		}
	}
}

func TestLogSearchState_RecomputeAndNavigation(t *testing.T) {
	r := newLogLineRing(8)
	for _, s := range []string{
		"server listening",
		"request id=abc tokens=128",
		"ERROR: connection reset",
		"request id=def tokens=64",
		"ERROR: slow token",
	} {
		r.Append(s)
	}
	var ss logSearchState
	ss.query = "error"
	ss.Recompute(r)

	if len(ss.matches) != 2 {
		t.Fatalf("matches=%v", ss.matches)
	}
	if ss.CurrentMatch() != 2 {
		t.Errorf("first match line=%d, want 2", ss.CurrentMatch())
	}
	if ss.NextMatch() != 4 {
		t.Errorf("next=%d, want 4", ss.CurrentMatch())
	}
	// Wrap.
	if ss.NextMatch() != 2 {
		t.Errorf("wrapped next=%d, want 2", ss.CurrentMatch())
	}
	if ss.PrevMatch() != 4 {
		t.Errorf("prev=%d, want 4", ss.CurrentMatch())
	}
}

func TestLogSearchState_EmptyQueryNoMatches(t *testing.T) {
	r := newLogLineRing(4)
	r.Append("hello")
	var ss logSearchState
	ss.Recompute(r)
	if len(ss.matches) != 0 {
		t.Errorf("want 0 matches for empty query, got %v", ss.matches)
	}
	if ss.CurrentMatch() != -1 || ss.NextMatch() != -1 || ss.PrevMatch() != -1 {
		t.Error("navigation should return -1 with no matches")
	}
}
