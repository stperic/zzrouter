package views

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
)

// ============================================================================
// Viewer input
// ============================================================================

func (m *RunlogsViewModel) updateViewer(msg tea.KeyPressMsg) tea.Cmd {
	// When the search prompt is open, all text input goes to it.
	if m.search.editing {
		return m.updateSearchPrompt(msg)
	}

	visible := m.viewerVisibleRows()

	switch {
	case key.Matches(msg, tui.RunlogsKeys.Search):
		m.search.active = true
		m.search.editing = true
		m.follow = m.pauseIfLive()
		return nil

	case key.Matches(msg, tui.RunlogsKeys.NextMatch):
		if m.search.active && len(m.search.matches) > 0 {
			line := m.search.NextMatch()
			m.centerOn(line, visible)
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.PrevMatch):
		if m.search.active && len(m.search.matches) > 0 {
			line := m.search.PrevMatch()
			m.centerOn(line, visible)
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Follow):
		if m.follow == followStopped {
			return nil
		}
		if m.follow == followLive {
			m.follow = followPaused
		} else {
			m.follow = followLive
			m.pendingNew = 0
			m.scrollToBottom()
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Refresh):
		// Hard reconnect: close the current session and build a new
		// one on the same run. Works for both running (reconnect) and
		// stopped (re-fetch tail) cases.
		m.closeSession()
		return m.enterViewer(m.run)

	case key.Matches(msg, tui.RunlogsKeys.Top):
		m.scrollTop = 0
		m.follow = m.pauseIfLive()
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Bottom):
		m.scrollToBottom()
		if m.follow != followStopped {
			m.follow = followLive
			m.pendingNew = 0
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Up):
		if m.scrollTop > 0 {
			m.scrollTop--
			m.follow = m.pauseIfLive()
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Down):
		m.scrollDownOne(visible)
		return nil

	case key.Matches(msg, tui.RunlogsKeys.PageUp):
		m.scrollTop -= visible
		if m.scrollTop < 0 {
			m.scrollTop = 0
		}
		m.follow = m.pauseIfLive()
		return nil

	case key.Matches(msg, tui.RunlogsKeys.PageDown):
		for i := 0; i < visible; i++ {
			m.scrollDownOne(visible)
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Home):
		m.scrollTop = 0
		m.follow = m.pauseIfLive()
		return nil

	case key.Matches(msg, tui.RunlogsKeys.End):
		m.scrollToBottom()
		if m.follow != followStopped {
			m.follow = followLive
			m.pendingNew = 0
		}
		return nil

	case key.Matches(msg, tui.RunlogsKeys.Back):
		if m.search.active {
			m.search.Reset()
			return nil
		}
		// Back to picker if we came from one, otherwise leave the view.
		if len(m.runs) > 1 {
			m.closeSession()
			m.mode = runlogsViewPicker
			return nil
		}
		return tui.NavBack()
	case key.Matches(msg, tui.RunlogsKeys.Quit):
		return tea.Quit
	}
	return nil
}

// updateSearchPrompt handles key input while the '/' prompt is active.
func (m *RunlogsViewModel) updateSearchPrompt(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.search.Reset()
		return nil
	case "enter":
		m.search.editing = false
		m.search.Recompute(m.ring)
		if line := m.search.CurrentMatch(); line >= 0 {
			m.centerOn(line, m.viewerVisibleRows())
		}
		return nil
	case "backspace":
		if n := len(m.search.query); n > 0 {
			m.search.query = m.search.query[:n-1]
		}
		return nil
	default:
		// Printable characters extend the query. Ignore everything
		// else (arrows, ctrl+*, etc.). Unicode-aware: accepts accented
		// letters, CJK, emoji — anything unicode.IsPrint considers
		// printable. Rejects multi-token bindings like "ctrl+a".
		s := msg.String()
		if s == "" || strings.ContainsRune(s, '+') {
			return nil
		}
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError || size != len(s) || !unicode.IsPrint(r) {
			return nil
		}
		m.search.query += s
		return nil
	}
}

// pauseIfLive transitions LIVE → PAUSED on user scroll; other states
// pass through unchanged.
func (m *RunlogsViewModel) pauseIfLive() followState {
	if m.follow == followLive {
		return followPaused
	}
	return m.follow
}

// scrollToBottom sets scrollTop so the last line of the ring sits on
// the last row of the viewport.
func (m *RunlogsViewModel) scrollToBottom() {
	visible := m.viewerVisibleRows()
	if m.ring.Len() <= visible {
		m.scrollTop = 0
		return
	}
	m.scrollTop = m.ring.Len() - visible
}

// scrollDownOne advances scrollTop by one line, clamping at the bottom.
// When the user scrolls off the bottom of a paused view we resume LIVE.
func (m *RunlogsViewModel) scrollDownOne(visible int) {
	maxTop := m.ring.Len() - visible
	if maxTop < 0 {
		maxTop = 0
	}
	if m.scrollTop >= maxTop {
		if m.follow != followStopped {
			m.follow = followLive
			m.pendingNew = 0
		}
		m.scrollTop = maxTop
		return
	}
	m.scrollTop++
}

// centerOn tries to place `line` in the middle of the viewport, pausing
// follow in the process.
func (m *RunlogsViewModel) centerOn(line, visible int) {
	if line < 0 {
		return
	}
	m.follow = m.pauseIfLive()
	target := line - visible/2
	if target < 0 {
		target = 0
	}
	maxTop := m.ring.Len() - visible
	if maxTop < 0 {
		maxTop = 0
	}
	if target > maxTop {
		target = maxTop
	}
	m.scrollTop = target
}

// viewerVisibleRows returns the number of log lines that fit in the
// current viewport, reserving space for the footer hints.
func (m *RunlogsViewModel) viewerVisibleRows() int {
	rows := m.termHeight - 2 // one line for status, one for hints separator
	if rows < 3 {
		return 3
	}
	return rows
}

// ============================================================================
// Viewer render
// ============================================================================

func (m *RunlogsViewModel) viewViewer(width int) string {
	var b strings.Builder

	// Header line (inside the child area, below the breadcrumb).
	title := fmt.Sprintf("%s · %s · %s @ %s", m.run.Provider, m.run.Model, shortID(m.run.ID), m.run.Node)
	badge := m.follow.Badge()
	headerLeft := " " + m.styles.DetailKey.Render(title)
	headerRight := m.styles.Accent.Render(badge) + " "
	gap := width - lipgloss.Width(headerLeft) - lipgloss.Width(headerRight)
	if gap < 1 {
		gap = 1
	}
	b.WriteString(headerLeft + strings.Repeat(" ", gap) + headerRight + "\n")

	// Body: log lines from the ring.
	visible := m.viewerVisibleRows()
	start := m.scrollTop
	if start > m.ring.Len()-1 {
		start = max(0, m.ring.Len()-visible)
		if start < 0 {
			start = 0
		}
	}
	end := start + visible
	if end > m.ring.Len() {
		end = m.ring.Len()
	}

	emptyLines := 0
	if start >= end {
		emptyLines = visible
	} else {
		for i := start; i < end; i++ {
			line := m.ring.At(i)
			line = renderLogLine(line, width-1, &m.search)
			b.WriteString(" " + line + "\n")
		}
		emptyLines = visible - (end - start)
	}
	for i := 0; i < emptyLines; i++ {
		b.WriteString("\n")
	}

	// Status line. A transient error takes precedence over the
	// default counters; it's cleared on the next successful EventEntry.
	var status string
	switch {
	case m.err != nil:
		status = m.styles.Error.Render(fmt.Sprintf("error: %v", m.err))
	case m.search.active && m.search.query != "":
		if len(m.search.matches) > 0 {
			status = fmt.Sprintf("match %d/%d  •  %q", m.search.cursor+1, len(m.search.matches), m.search.query)
		} else {
			status = fmt.Sprintf("no matches for %q", m.search.query)
		}
	default:
		status = fmt.Sprintf("%d lines  •  buffer %d/%d  •  follow: %s",
			m.totalLines, m.ring.Len(), m.ring.Cap(), followLabel(m.follow))
		if m.pendingNew > 0 {
			status += fmt.Sprintf("  •  +%d new", m.pendingNew)
		}
	}

	// Footer hints.
	var hints string
	if m.search.editing {
		hints = fmt.Sprintf("/ %s_    Enter confirm  Esc cancel", m.search.query)
	} else {
		parts := []string{
			tui.Hint(tui.RunlogsKeys.Up, "scroll"),
			tui.Hint(tui.RunlogsKeys.PageUp, "page"),
			tui.Hint(tui.RunlogsKeys.Top, "top"),
			tui.Hint(tui.RunlogsKeys.Bottom, "bottom"),
		}
		if m.follow != followStopped {
			parts = append(parts, tui.Hint(tui.RunlogsKeys.Follow, "follow"))
		}
		parts = append(parts,
			tui.Hint(tui.RunlogsKeys.Refresh, "reconnect"),
			tui.Hint(tui.RunlogsKeys.Search, "search"),
			tui.Hint(tui.RunlogsKeys.Back, "back"),
		)
		hints = tui.JoinHints(parts...)
	}

	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, hints))
	return b.String()
}

// ============================================================================
// Rendering helpers
// ============================================================================

// renderLogLine applies search-match highlighting and truncates to the
// available display width. Simple substring highlight — no fancy layout.
func renderLogLine(line string, width int, s *logSearchState) string {
	// Truncate plain text first (highlight may add ANSI bytes that
	// break width calculations, so we do width control on raw text).
	// Rune- and width-aware: uses lipgloss.Width so CJK double-width
	// and emoji don't split mid-glyph.
	if width > 0 && lipgloss.Width(line) > width {
		line = truncateToDisplayWidth(line, width)
	}
	if s == nil || !s.active || s.query == "" {
		return line
	}
	// Case-insensitive substring highlight. Single pass to avoid
	// breaking overlapping matches.
	lower := strings.ToLower(line)
	q := strings.ToLower(s.query)
	if !strings.Contains(lower, q) {
		return line
	}
	var b strings.Builder
	highlightStyle := lipgloss.NewStyle().Reverse(true)
	i := 0
	for i < len(line) {
		idx := strings.Index(lower[i:], q)
		if idx < 0 {
			b.WriteString(line[i:])
			break
		}
		b.WriteString(line[i : i+idx])
		matchEnd := i + idx + len(q)
		b.WriteString(highlightStyle.Render(line[i+idx : matchEnd]))
		i = matchEnd
	}
	return b.String()
}

// truncateToDisplayWidth rune-slices s so that its lipgloss-measured
// display width is <= max. Unlike shared.TruncateText it adds no ellipsis —
// raw log lines get a hard visual cut. Binary search on rune index.
func truncateToDisplayWidth(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lipgloss.Width(string(runes[:mid])) <= max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

// followLabel returns a lowercase label for the footer status line.
func followLabel(s followState) string {
	switch s {
	case followLive:
		return "on"
	case followPaused:
		return "off"
	case followStopped:
		return "stopped"
	}
	return "?"
}
