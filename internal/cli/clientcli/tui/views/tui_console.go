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
	grant  *pkgClient.ConsoleGrant
}

// ConsoleViewModel is a normal navigation-stack view, so launching a browser
// never exits the TUI and returning restores the operator's previous selection.
type ConsoleViewModel struct {
	tui.ViewContext
	styles           ui.Styles
	target           pkgClient.ConsoleTarget
	discover         func(context.Context, pkgClient.ConsoleTarget) (string, error)
	handoff          func(context.Context, pkgClient.ConsoleTarget) (*pkgClient.ConsoleGrant, error)
	open             func(string) error
	copy             func(string) error
	browserAvailable func() bool
	width            int
	height           int
	detail           tui.Detail
	url              string
	status           string
	code             string
	loading          bool
	busy             bool
	err              error
}

// NewConsoleViewModel creates a contextual handoff without opening the browser.
func NewConsoleViewModel(client *pkgClient.Client, styles ui.Styles, target pkgClient.ConsoleTarget) *ConsoleViewModel {
	return &ConsoleViewModel{
		styles: styles, target: target, loading: true,
		discover: client.ConsoleURL, open: shared.OpenFile,
		handoff: client.BeginConsoleHandoff,
		copy:    clipboard.WriteAll, browserAvailable: shared.BrowserAvailable,
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
		if msg.grant != nil {
			m.code = msg.grant.Token
		}
		if msg.err != nil {
			m.status = fmt.Sprintf("Could not %s: %v. Use the URL above.", msg.action, msg.err)
		} else if msg.action == "copy" {
			m.status = "Console URL copied."
		} else {
			m.status = "Paste the temporary code into 'Use a temporary code' in the console."
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
			m.code = ""
			return m, tui.NavBack()
		case "ctrl+c":
			return m, tea.Quit
		case "r":
			if !m.loading && !m.busy {
				m.loading, m.err, m.status, m.code = true, nil, "", ""
				return m, m.Init()
			}
		case "enter", "o", "c", "y":
			return m, m.consoleActionCmd(msg.String())
		}
	}
	return m, nil
}

func (m *ConsoleViewModel) consoleActionCmd(key string) tea.Cmd {
	if m.loading || m.busy || m.url == "" {
		return nil
	}
	action, effect := "open", m.open
	if strings.EqualFold(key, "c") || strings.EqualFold(key, "y") {
		action, effect = "copy", m.copy
	}
	m.busy = true
	ctx, consoleURL := m.Context(), m.url
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		var grant *pkgClient.ConsoleGrant
		if action == "open" {
			var err error
			grant, err = m.handoff(ctx, m.target)
			if err != nil {
				return consoleActionMsg{owner: m, action: "authorize console entry", err: err}
			}
			if ctx.Err() != nil {
				return nil
			}
			if !m.browserAvailable() {
				return consoleActionMsg{owner: m, action: "authorize", grant: grant}
			}
		}
		err := effect(consoleURL)
		if ctx.Err() != nil {
			return nil
		}
		return consoleActionMsg{owner: m, action: action, err: err, grant: grant}
	}
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
		d.Text("  ", "Open to get a temporary sign-in code. Copying the URL does not create one.")
		if m.code != "" {
			d.Write("\n")
			d.Text("  Temporary code: ", m.code)
		}
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
