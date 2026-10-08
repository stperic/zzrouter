package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// SpinnerFrames is a simple braille-dot spinner shared across the
// client + server TUIs.
var SpinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// SpinnerFrame returns the spinner character for the given tick count.
func SpinnerFrame(tick int) string {
	return SpinnerFrames[tick%len(SpinnerFrames)]
}

// SpinnerTickMsg advances the loading spinner.
type SpinnerTickMsg struct{}

// SpinnerTickCmd returns a command that sends a SpinnerTickMsg after
// 80ms — the shared cadence for spinner animations across the TUIs.
func SpinnerTickCmd() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(_ time.Time) tea.Msg {
		return SpinnerTickMsg{}
	})
}
