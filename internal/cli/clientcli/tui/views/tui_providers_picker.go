package views

import (
	"fmt"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
)

func (m *ProvidersViewModel) updatePicker(msg tea.KeyPressMsg) tea.Cmd { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch {
	case key.Matches(msg, tui.ProvidersKeys.Back):
		switch m.mode {
		case providersViewPickNode:
			m.buildProviderPicker()
		case providersViewPickProvider:
			m.buildTypePicker()
		default:
			m.mode = providersViewList
		}
	case key.Matches(msg, tui.ProvidersKeys.Up):
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
	case key.Matches(msg, tui.ProvidersKeys.Down):
		if m.pickerCursor < len(m.pickerItems)-1 {
			m.pickerCursor++
		}
	case key.Matches(msg, tui.ProvidersKeys.Enter):
		if m.pickerCursor >= len(m.pickerKeys) {
			break
		}
		switch m.mode {
		case providersViewPickType:
			// Type selected — fetch catalog filtered by type
			m.installType = m.pickerKeys[m.pickerCursor]
			m.loading = true
			client := m.client
			typeFilter := m.installType
			return tea.Batch(func() tea.Msg {
				catalog, err := client.GetProviderCatalog()
				// Filter client-side based on type
				if err == nil {
					var filtered []pkgClient.ProviderCatalogEntry
					for _, e := range catalog {
						if typeFilter == constants.AppModeCloud && e.IsCloud {
							filtered = append(filtered, e)
						} else if typeFilter == "local" && !e.IsCloud {
							filtered = append(filtered, e)
						}
					}
					catalog = filtered
				}
				return providersCatalogMsg{catalog: catalog, err: err}
			}, shared.SpinnerTickCmd())

		case providersViewPickProvider:
			// Provider selected — check status from catalog
			name := m.pickerKeys[m.pickerCursor]
			m.installProvider = name

			// Synthetic "connect a remote Ollama" entry — route into the
			// form flow and skip the catalog/node picker entirely.
			if name == ollamaConnectCatalogKey {
				m.initConnectForm()
				m.mode = providersViewConnectForm
				return m.focusConnectInput(connectFieldName)
			}

			// Find catalog entry for status checks
			var entry *pkgClient.ProviderCatalogEntry
			for i := range m.catalog {
				if m.catalog[i].Key == name {
					entry = &m.catalog[i]
					break
				}
			}

			if m.installType == constants.AppModeCloud {
				// Cloud providers are cluster-wide — if already enabled, nothing to do.
				if entry != nil && entry.Installed {
					m.actionStatus = fmt.Sprintf("✓ %s is already installed and ready to go", name)
					m.mode = providersViewList
					return nil
				}

				// Cloud — check if env vars are available
				if entry != nil && !entry.Available && len(entry.RequiredEnvVars) > 0 {
					// Env vars missing — prompt user to enter them
					m.keyEnvVars = entry.RequiredEnvVars
					m.keyEnvIndex = 0
					m.keyInput = newEnvVarInput(m.keyEnvVars[0])
					m.mode = providersViewEnterKey
					return m.keyInput.Focus()
				}
				// Cloud with key — verify connectivity and enable
				m.actionStatus = fmt.Sprintf("Verifying %s...", name)
				m.mode = providersViewList
				client := m.client
				provider := name
				return func() tea.Msg {
					result, err := client.VerifyProvider(provider)
					return providersVerifyMsg{result: result, err: err}
				}
			}

			// Local — fetch cluster nodes, exclude nodes that already have this provider
			nodes, err := shared.FetchNodes(m.client, "name", false)
			if err != nil {
				m.actionStatus = fmt.Sprintf("Failed to fetch nodes: %v", err)
				m.mode = providersViewList
				return nil
			}
			installed := make(map[string]bool)
			for _, app := range m.providers {
				if app.Name == name {
					installed[app.Node] = true
				}
			}
			var items, keys []string
			for _, node := range nodes {
				if !installed[node.Name] {
					items = append(items, node.Name)
					keys = append(keys, node.Name)
				}
			}
			if len(items) == 0 {
				m.actionStatus = fmt.Sprintf("No available nodes for %s", name)
				m.mode = providersViewList
				return nil
			}
			m.pickerItems = items
			m.pickerKeys = keys
			m.pickerCursor = 0
			m.pickerTitle = fmt.Sprintf("Install %s on", name)
			m.mode = providersViewPickNode

		case providersViewPickNode:
			// Node selected — run preflight checks first
			m.installNode = m.pickerKeys[m.pickerCursor]
			m.installPlan = nil
			m.actionStatus = ""
			m.loading = true
			return tea.Batch(
				fetchPreflight(m.client, m.installProvider, m.installNode),
				shared.SpinnerTickCmd(),
			)
		}
	}
	return nil
}

func (m *ProvidersViewModel) buildTypePicker() {
	m.pickerItems = []string{
		"Cloud Providers (gemini, openai, groq, ...)",
		"Local Providers (ollama, vllm, llamacpp, mlx)",
	}
	m.pickerKeys = []string{constants.AppModeCloud, "local"}
	m.pickerCursor = 0
	m.pickerTitle = "Install Provider"
	m.mode = providersViewPickType
}

func (m *ProvidersViewModel) buildProviderPicker() {
	var items, keys []string
	for _, entry := range m.catalog {
		items = append(items, entry.Key)
		keys = append(keys, entry.Key)
	}
	// Inject the synthetic "connect a remote Ollama" entry at the bottom
	// of the Local picker. It isn't a real catalog entry — selecting it
	// routes into the connect form rather than the normal install flow.
	if m.installType == "local" {
		items = append(items, "ollama-connect  (connect a remote Ollama daemon)")
		keys = append(keys, ollamaConnectCatalogKey)
	}
	m.pickerItems = items
	m.pickerKeys = keys
	m.pickerCursor = 0
	if m.installType == constants.AppModeCloud {
		m.pickerTitle = "Cloud Providers"
	} else {
		m.pickerTitle = "Local Providers"
	}
	m.mode = providersViewPickProvider
}
