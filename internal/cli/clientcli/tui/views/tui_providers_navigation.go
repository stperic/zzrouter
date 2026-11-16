package views

import (
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	logsclient "github.com/stperic/zzrouter/internal/client/logs"
)

func (m *ProvidersViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd {
	if m.uninstallTarget != nil {
		return m.updateUninstallConfirm(msg)
	}
	if handled, cmd := m.updateProviderFilter(msg); handled {
		return cmd
	}

	if key.Matches(msg, tui.ProvidersKeys.Dependencies) {
		return m.openSelectedProviderComponents()
	}
	// If a sub-view (install/breadcrumb) flagged a refresh, trigger it now
	if m.needsRefresh && !m.loading {
		m.needsRefresh = false
		m.loading = true
		return tea.Batch(m.fetchProvidersCmd(true), shared.SpinnerTickCmd())
	}

	if m.list.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.ProvidersKeys.Enter):
		if app := m.selectedProvider(); app != nil {
			m.mode = providersViewDetail
			m.detailParams = nil
			m.detailParamsErr = nil
			m.detailParamsLoading = true
			m.detail.Reset()
			m.providerStatus = nil
			m.versionsReport = nil
			client := m.client
			appName := app.Name
			return tea.Batch(
				m.fetchProviderVersions(appName, false),
				func() tea.Msg {
					resp, err := client.GetProviderParameters(appName, true)
					if err != nil {
						return providersDetailParamsMsg{err: err}
					}
					return providersDetailParamsMsg{params: resp.Parameters}
				},
				func() tea.Msg {
					statuses, err := client.ListProviderStatus()
					if err != nil {
						return providersStatusMsg{}
					}
					for i := range statuses {
						if statuses[i].Name == appName {
							return providersStatusMsg{status: &statuses[i]}
						}
					}
					return providersStatusMsg{}
				},
			)
		}
		return nil
	case key.Matches(msg, tui.ProvidersKeys.Install):
		m.actionStatus = ""
		m.buildTypePicker()
		return nil

	case key.Matches(msg, tui.ProvidersKeys.Uninstall):
		app := m.selectedProvider()
		if app == nil {
			break
		}
		if m.uninstallUncertain {
			m.actionStatus = "Uninstall outcome unknown. Refresh providers before trying again."
			return nil
		}
		target := *app
		m.uninstallTarget = &target
		return nil

	case key.Matches(msg, tui.ProvidersKeys.Upgrade):
		app := m.selectedProvider()
		if app == nil {
			break
		}
		m.actionStatus = ""
		m.upgradeProvider, m.upgradeNode, m.upgradeCurrent = app.Name, app.Node, app.Version
		m.upgradeLatest, m.upgradeStatus = app.LatestVersion, app.VersionStatus
		m.upgradePin = false
		m.upgradeInput = newVersionInput()
		m.prefillUpgradeInput(app)
		m.mode = providersViewUpgrade
		return m.upgradeInput.Focus()

	case key.Matches(msg, tui.ProvidersKeys.Logs):
		app := m.selectedProvider()
		if app == nil {
			break
		}
		providerName := app.Name
		return func() tea.Msg {
			return tui.LaunchRunLogsMsg{Filter: logsclient.RunFilter{Provider: providerName}}
		}

	case key.Matches(msg, tui.ProvidersKeys.Refresh):
		m.loading = true
		m.actionStatus = ""
		return m.fetchProvidersCmd(true)
	case key.Matches(msg, tui.ProvidersKeys.Back):
		return tui.NavBack()
	case key.Matches(msg, tui.ProvidersKeys.Quit):
		return tea.Quit
	}

	return nil
}

func (m *ProvidersViewModel) updateDetail(msg tea.KeyPressMsg) tea.Cmd {
	if m.detail.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.ProvidersKeys.Dependencies):
		return m.openSelectedProviderComponents()
	case key.Matches(msg, tui.ProvidersKeys.Edit):
		if app := m.selectedProvider(); app != nil {
			appName, node := app.Name, app.Node
			return func() tea.Msg {
				return tui.OpenParamsEditorMsg{Provider: appName, Node: node}
			}
		}
		return nil
	case key.Matches(msg, tui.ProvidersKeys.Back, tui.ProvidersKeys.Enter):
		m.mode = providersViewList
		return nil
	case key.Matches(msg, tui.ProvidersKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *ProvidersViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if m.uninstallTarget != nil || m.filterInput.Focused() {
		return nil
	}
	if msg.Button == tea.MouseRight {
		if m.mode == providersViewDetail {
			m.mode = providersViewList
			m.detail.Reset()
		} else {
			return tui.NavBack()
		}
		return nil
	}
	if m.mode != providersViewList {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if !ok || !enter {
		return nil
	}
	m.mode = providersViewDetail
	m.detailParams = nil
	m.detailParamsErr = nil
	m.detailParamsLoading = true
	m.detail.Reset()
	m.versionsReport = nil
	client := m.client
	appName := m.providers[m.list.Cursor()].Name
	return tea.Batch(
		m.fetchProviderVersions(appName, false),
		func() tea.Msg {
			resp, err := client.GetProviderParameters(appName, true)
			if err != nil {
				return providersDetailParamsMsg{err: err}
			}
			return providersDetailParamsMsg{params: resp.Parameters}
		},
	)
}

func (m *ProvidersViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	if m.uninstallTarget != nil {
		return
	}
	if m.mode == providersViewList {
		m.list.UpdateWheel(msg)
	}
}

// Breadcrumb implements tui.Breadcrumber.
func (m *ProvidersViewModel) Breadcrumb() []string {
	switch m.mode {
	case providersViewDetail:
		if p := m.selectedProvider(); p != nil {
			return []string{"Providers", p.Name}
		}
		return []string{"Providers", "Details"}
	case providersViewPickType, providersViewPickProvider, providersViewPickNode:
		return []string{"Providers", "Install"}
	case providersViewPreflightFail:
		return []string{"Providers", "Install", "Preflight"}
	case providersViewInstallSteps:
		if m.installPlan != nil {
			return []string{"Providers", "Install", m.installPlan.Provider}
		}
		return []string{"Providers", "Install"}
	case providersViewEnterKey:
		return []string{"Providers", "Install", m.installProvider}
	case providersViewConnectForm:
		return []string{"Providers", "Add remote Ollama"}
	default:
		return []string{"Providers"}
	}
}

// navigateToBreadcrumb navigates to the state matching breadcrumb level.
func (m *ProvidersViewModel) navigateToBreadcrumb(level int) {
	m.retireInstallRequest()
	if m.mode == providersViewEnterKey {
		m.cancelConnection()
	}
	switch level {
	case 1:
		// Level 1 is detail or install — go back to list
		m.mode = providersViewList
	case 2:
		// Level 2 is install sub-step — go back to install picker
		m.mode = providersViewPickProvider
	}
}

// View implements tea.Model.
func (m *ProvidersViewModel) View() tea.View {
	hints := tui.ProvidersKeys.ListHintsString() + "  / search  Tab local/cloud  N node  X disabled"
	if m.mode == providersViewDetail {
		hints = tui.ProvidersKeys.DetailHintsString()
	}
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.help.View(m.styles, m.viewContent(), m.termWidth, m.termHeight, hints))
}

func (m *ProvidersViewModel) viewContent() string {
	width, height := m.termWidth, m.termHeight
	switch m.mode {
	case providersViewDetail:
		return m.viewDetail(width, height)
	case providersViewPickType, providersViewPickProvider, providersViewPickNode:
		return m.viewPicker(width, height)
	case providersViewPreflightFail:
		return m.viewPreflightFail(width, height)
	case providersViewInstallSteps:
		return m.viewInstallWizard(width, height)
	case providersViewEnterKey:
		return m.viewEnterKey(width, height)
	case providersViewConnectForm:
		return m.viewConnectForm(width, height)
	case providersViewUpgrade:
		return m.viewUpgrade(width, height)
	default:
		return m.viewList(width, height)
	}
}
