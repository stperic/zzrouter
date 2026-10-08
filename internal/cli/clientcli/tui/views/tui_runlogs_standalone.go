package views

// Standalone TUI host for the run logs screen. Used by the CLI
// (`zzrouter run logs …`) to launch the run logs view as a one-shot
// program, without the full menu-rooted tuiModel.
//
// Implementation: wrap the runlogs view in a tui.Root with a single
// frame. NavBack from the view pops to empty, which we observe in our
// Update wrapper and translate to tea.Quit.

import (
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	logsclient "github.com/stperic/zzrouter/internal/client/logs"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

type RunlogsStandaloneModel struct {
	root *tui.Root
}

func newRunlogsStandaloneModel(client *pkgClient.Client, filter logsclient.RunFilter) *RunlogsStandaloneModel {
	styles := ui.NewStyles(shared.ResolveUITheme())
	view := NewRunlogsViewModel(client, styles, filter)
	return &RunlogsStandaloneModel{root: tui.NewRoot(view)}
}

func (m *RunlogsStandaloneModel) Init() tea.Cmd {
	return m.root.Init()
}

func (m *RunlogsStandaloneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyPressMsg); ok && km.String() == "ctrl+c" {
		return m, tea.Quit
	}
	updated, cmd := m.root.Update(msg)
	m.root, _ = updated.(*tui.Root)
	if m.root.Depth() == 0 {
		return m, tea.Quit
	}
	return m, cmd
}

func (m *RunlogsStandaloneModel) View() tea.View {
	return m.root.View()
}

// runRunlogsStandaloneTUI launches the standalone program. Blocks until
// the user quits. Returns any error from tea.Program.
func RunRunlogsStandaloneTUI(client *pkgClient.Client, filter logsclient.RunFilter) error {
	m := newRunlogsStandaloneModel(client, filter)
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
