package ui

import "charm.land/lipgloss/v2"

// Styles holds pre-computed lipgloss styles derived from a Theme.
type Styles struct {
	Theme Theme // The underlying theme (for raw color access)
	// Shared
	Title       lipgloss.Style
	Selected    lipgloss.Style
	Normal      lipgloss.Style
	Error       lipgloss.Style
	Success     lipgloss.Style
	Help        lipgloss.Style // Dim gray for separators, faint UI
	HintsBar    lipgloss.Style // Normal color for footer hints/info text
	DetailKey   lipgloss.Style
	DetailValue lipgloss.Style
	Accent      lipgloss.Style

	// Status — muted colors for table row status differentiation
	StatusRunning     lipgloss.Style // running/healthy
	StatusIdle        lipgloss.Style // idle/available
	StatusDownloading lipgloss.Style // downloading/in-progress
	StatusError       lipgloss.Style // failed/error/offline

	// Chat
	ChatUser         lipgloss.Style
	ChatAssistant    lipgloss.Style
	ChatThinking     lipgloss.Style
	ChatThinkingBox  lipgloss.Style
	ChatStatusBar    lipgloss.Style
	ChatStatusAccent lipgloss.Style
	ChatStatusDim    lipgloss.Style
	ChatDim          lipgloss.Style
}

// NewStyles creates a Styles from the given Theme.
func NewStyles(t Theme) Styles {
	return Styles{
		Theme:       t,
		Title:       lipgloss.NewStyle().Bold(true).Foreground(t.Primary).Padding(0, 1),
		Selected:    lipgloss.NewStyle().Foreground(t.Text).Background(t.Surface).Bold(true),
		Normal:      lipgloss.NewStyle().Foreground(t.Text),
		Error:       lipgloss.NewStyle().Foreground(t.Error).Bold(true),
		Success:     lipgloss.NewStyle().Foreground(t.Success).Bold(true),
		Help:        lipgloss.NewStyle().Foreground(t.Overlay),
		HintsBar:    lipgloss.NewStyle().Foreground(t.Subtext),
		DetailKey:   lipgloss.NewStyle().Foreground(t.Subtext).Bold(true),
		DetailValue: lipgloss.NewStyle().Foreground(t.DetailKey),
		Accent:      lipgloss.NewStyle().Foreground(t.Accent).Bold(true),

		StatusRunning:     lipgloss.NewStyle().Foreground(t.Success),
		StatusIdle:        lipgloss.NewStyle().Foreground(t.Overlay),
		StatusDownloading: lipgloss.NewStyle().Foreground(t.Info),
		StatusError:       lipgloss.NewStyle().Foreground(t.Error),

		ChatUser:         lipgloss.NewStyle().Foreground(t.Primary).Bold(true),
		ChatAssistant:    lipgloss.NewStyle().Foreground(t.Success).Bold(true),
		ChatThinking:     lipgloss.NewStyle().Foreground(t.Overlay).Italic(true),
		ChatThinkingBox:  lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(t.Overlay).PaddingLeft(1),
		ChatStatusBar:    lipgloss.NewStyle().Foreground(t.Subtext),
		ChatStatusAccent: lipgloss.NewStyle().Foreground(t.Secondary).Bold(true),
		ChatStatusDim:    lipgloss.NewStyle().Foreground(t.Overlay),
		ChatDim:          lipgloss.NewStyle().Foreground(t.Overlay),
	}
}
