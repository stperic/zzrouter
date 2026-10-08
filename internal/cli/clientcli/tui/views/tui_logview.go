package views

// Shared primitives for log-viewer TUI screens. Small, self-contained
// building blocks used by tui_runlogs.go today and available for a
// future rewrite of the inference-logs screen onto the same bones.
//
// None of these types know about run logs or inference logs — they
// handle raw strings, counts, and state transitions.

import (
	"strings"
)

// ============================================================================
// logLineRing — capacity-bounded ring buffer of log lines.
// ============================================================================

const logViewDefaultCap = 1000

type logLineRing struct {
	buf   []string
	start int // index of oldest line
	len   int
	cap   int
}

func newLogLineRing(capacity int) *logLineRing {
	if capacity <= 0 {
		capacity = logViewDefaultCap
	}
	return &logLineRing{buf: make([]string, capacity), cap: capacity}
}

func (r *logLineRing) Len() int { return r.len }
func (r *logLineRing) Cap() int { return r.cap }

// Append adds a line, evicting the oldest entry when at capacity.
// Returns true if an eviction occurred (consumers may need to adjust
// absolute line indices).
func (r *logLineRing) Append(line string) (evicted bool) {
	if r.len < r.cap {
		r.buf[(r.start+r.len)%r.cap] = line
		r.len++
		return false
	}
	r.buf[r.start] = line
	r.start = (r.start + 1) % r.cap
	return true
}

// At returns the line at logical index i (0 = oldest). Panics on
// out-of-range, same as slice indexing; callers are expected to bound-
// check against Len().
func (r *logLineRing) At(i int) string {
	return r.buf[(r.start+i)%r.cap]
}

// Snapshot returns a flat slice copy in logical order. Allocates; use
// sparingly — the hot path should use At() and Len().
func (r *logLineRing) Snapshot() []string {
	out := make([]string, r.len)
	for i := 0; i < r.len; i++ {
		out[i] = r.At(i)
	}
	return out
}

// Reset empties the ring without releasing capacity. Used on
// EventReset from a log session (rotation fallback).
func (r *logLineRing) Reset() {
	r.start = 0
	r.len = 0
}

// ============================================================================
// followState — LIVE / PAUSED / STOPPED state machine.
// ============================================================================

type followState int

const (
	// followLive: viewport pins to the newest entry; new lines push
	// the view as they arrive.
	followLive followState = iota
	// followPaused: user scrolled away; new lines accumulate in the
	// buffer but the viewport does not move.
	followPaused
	// followStopped: the underlying run has ended. Follow is no
	// longer available; the view becomes a static tail.
	followStopped
)

func (s followState) String() string {
	switch s {
	case followLive:
		return "LIVE"
	case followPaused:
		return "PAUSED"
	case followStopped:
		return "STOPPED"
	}
	return "UNKNOWN"
}

// Badge returns the status indicator rendered in the viewer header.
// The glyphs are plain ASCII-ish so they work in every terminal.
func (s followState) Badge() string {
	switch s {
	case followLive:
		return "● LIVE"
	case followPaused:
		return "⏸ PAUSED"
	case followStopped:
		return "⏹ STOPPED"
	}
	return ""
}

// ============================================================================
// logSearchState — in-viewer '/' search over the buffer.
// ============================================================================

type logSearchState struct {
	// active reports whether a search is in progress. When false the
	// viewer renders normally and match*/query fields are ignored.
	active bool
	// editing reports whether the footer prompt is currently capturing
	// input. When true, key events go to the prompt instead of the
	// viewport.
	editing bool
	// query is the current search string. Empty = no matches.
	query string
	// matches is the sorted list of logical line indices that contain
	// the query. Recomputed whenever the query changes or new lines
	// append to the buffer.
	matches []int
	// cursor is the index into matches[] of the currently focused hit.
	cursor int
}

// Reset clears the search without changing the sub-mode (the user's
// preference for Highlight vs Filter persists).
func (s *logSearchState) Reset() {
	s.active = false
	s.editing = false
	s.query = ""
	s.matches = s.matches[:0]
	s.cursor = 0
}

// matchLine reports whether line contains the search query (case-
// insensitive). An empty query matches nothing, not everything —
// callers should gate rendering of match decorations on len(query)>0.
func (s *logSearchState) matchLine(line string) bool {
	if s.query == "" {
		return false
	}
	return strings.Contains(strings.ToLower(line), strings.ToLower(s.query))
}

// Recompute scans the ring from line 0 and refreshes the match list
// against the current query. Called on query edit, on Append (from
// the viewer), and on Reset().
func (s *logSearchState) Recompute(ring *logLineRing) {
	s.matches = s.matches[:0]
	if s.query == "" {
		s.cursor = 0
		return
	}
	for i := 0; i < ring.Len(); i++ {
		if s.matchLine(ring.At(i)) {
			s.matches = append(s.matches, i)
		}
	}
	if s.cursor >= len(s.matches) {
		s.cursor = 0
	}
}

// NextMatch advances the match cursor, wrapping to 0 at the end.
// Returns the new line index, or -1 if no matches exist.
func (s *logSearchState) NextMatch() int {
	if len(s.matches) == 0 {
		return -1
	}
	s.cursor = (s.cursor + 1) % len(s.matches)
	return s.matches[s.cursor]
}

// PrevMatch is NextMatch in reverse.
func (s *logSearchState) PrevMatch() int {
	if len(s.matches) == 0 {
		return -1
	}
	s.cursor = (s.cursor - 1 + len(s.matches)) % len(s.matches)
	return s.matches[s.cursor]
}

// CurrentMatch returns the line index of the focused hit, or -1.
func (s *logSearchState) CurrentMatch() int {
	if len(s.matches) == 0 {
		return -1
	}
	return s.matches[s.cursor]
}
