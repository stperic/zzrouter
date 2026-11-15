package views

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

type consoleResultMsg struct {
	owner *ConsoleViewModel
	url   string
	err   error
}

type consoleActionMsg struct {
	owner  *ConsoleViewModel
	action string
	err    error
}

// ConsoleViewModel is a normal navigation-stack view, so launching a browser
// never exits the TUI and returning restores the operator's previous selection.
type ConsoleViewModel struct {
	tui.ViewContext
	styles           ui.Styles
	target           pkgClient.ConsoleTarget
	discover         func(context.Context, pkgClient.ConsoleTarget) (string, error)
	open             func(string) error
	copy             func(string) error
	browserAvailable func() bool
	width            int
	height           int
	detail           tui.Detail
	url              string
	status           string
	loading          bool
	busy             bool
	err              error
}

// NewConsoleViewModel creates a contextual handoff without opening the browser.
func NewConsoleViewModel(client *pkgClient.Client, styles ui.Styles, target pkgClient.ConsoleTarget) *ConsoleViewModel {
	return &ConsoleViewModel{
		styles: styles, target: target, loading: true,
		discover: client.ConsoleURL, open: shared.OpenFile,
		copy: clipboard.WriteAll, browserAvailable: shared.BrowserAvailable,
		width: 80, height: 20, detail: tui.NewDetail(),
	}
}

// Init discovers the public URL during the lifetime of this view.
func (m *ConsoleViewModel) Init() tea.Cmd {
	ctx := m.Context()
	return func() tea.Msg {
		consoleURL, err := m.discover(ctx, m.target)
		if ctx.Err() != nil {
			return nil
		}
		return consoleResultMsg{owner: m, url: consoleURL, err: err}
	}
}

// Breadcrumb identifies the browser handoff in the navigation stack.
func (m *ConsoleViewModel) Breadcrumb() []string { return []string{"Web console"} }

// Update handles discovery and explicit browser or clipboard actions.
func (m *ConsoleViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.Context().Err() != nil {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
	case tea.MouseWheelMsg:
		m.detail.UpdateWheel(msg)
	case consoleResultMsg:
		if msg.owner != m || m.Context().Err() != nil {
			return m, nil
		}
		m.loading, m.url, m.err = false, msg.url, msg.err
	case consoleActionMsg:
		if msg.owner != m || m.Context().Err() != nil {
			return m, nil
		}
		m.busy = false
		if msg.err != nil {
			m.status = fmt.Sprintf("Could not %s: %v. Use the URL above.", msg.action, msg.err)
		} else if msg.action == "copy" {
			m.status = "Console URL copied."
		} else {
			m.status = "Browser launch requested. Sign in there with your API key."
		}
	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
	case tea.KeyPressMsg:
		if m.detail.UpdateKey(msg) {
			return m, nil
		}
		switch strings.ToLower(msg.String()) {
		case "esc", "backspace", "q":
			return m, tui.NavBack()
		case "ctrl+c":
			return m, tea.Quit
		case "r":
			if !m.loading && !m.busy {
				m.loading, m.err, m.status = true, nil, ""
				return m, m.Init()
			}
		case "enter", "o", "c", "y":
			if m.loading || m.busy || m.url == "" {
				return m, nil
			}
			action, effect := "open", m.open
			if strings.EqualFold(msg.String(), "c") || strings.EqualFold(msg.String(), "y") {
				action, effect = "copy", m.copy
			} else if !m.browserAvailable() {
				m.status = "Open this URL in a browser on your computer. This terminal is remote or headless."
				return m, nil
			}
			m.busy = true
			ctx, consoleURL := m.Context(), m.url
			return m, func() tea.Msg {
				if ctx.Err() != nil {
					return nil
				}
				err := effect(consoleURL)
				if ctx.Err() != nil {
					return nil
				}
				return consoleActionMsg{owner: m, action: action, err: err}
			}
		}
	}
	return m, nil
}

// View renders the URL and action hints within the terminal viewport.
func (m *ConsoleViewModel) View() tea.View {
	d := shared.NewDetail(m.styles, m.width)
	d.Section("Open web console")
	d.Text("  ", "Manage the full cluster in your browser. Your terminal session stays open.")
	d.Write("\n")
	switch {
	case m.loading:
		d.Text("  ", "Finding the coordinator's public console URL...")
	case m.err != nil:
		d.Text("  ", m.styles.Error.Render(m.err.Error()))
	default:
		d.Text("  ", m.url)
		d.Write("\n")
		d.Text("  ", "Sign in using your API key. The URL contains no credentials.")
	}
	if m.status != "" {
		d.Write("\n")
		d.Text("  ", m.status)
	}
	if m.busy {
		d.Text("  ", "Working...")
	}
	hints := shared.WrapIndent("Enter/O open  C copy URL  R refresh  ↑/↓ scroll  Esc back", "", max(m.width-2, 1))
	footer := shared.RenderViewFooter(m.styles, m.width, hints)
	content := m.detail.Render(d.String(), max(m.height-strings.Count(footer, "\n")-2, 1)) + "\n" + footer
	return shared.RenderChildView(m.styles, m.width, m.Breadcrumb(), content)
}
