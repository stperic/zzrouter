package views

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/pkg/constants"
)

func providerKind(p shared.ProviderInfo) string {
	kind := p.Kind
	if kind == "" {
		kind = p.Mode
	}
	if kind == constants.AppModeCloud {
		return "Cloud"
	}
	return "Local"
}

func (m *ProvidersViewModel) filterProviders() {
	selected := ""
	if p := m.selectedProvider(); p != nil {
		selected = p.Name + "\x00" + p.Node
	}
	m.providers = nil
	for _, p := range m.allProviders {
		if !m.showDisabled && !p.Enabled {
			continue
		}
		if m.filterKind == 1 && providerKind(p) != "Local" || m.filterKind == 2 && providerKind(p) != "Cloud" {
			continue
		}
		if m.filterNode != "" && p.Node != m.filterNode {
			continue
		}
		if !strings.Contains(strings.ToLower(p.Name+" "+p.Node+" "+providerKind(p)), strings.ToLower(strings.TrimSpace(m.filterInput.Value()))) {
			continue
		}
		m.providers = append(m.providers, p)
	}
	slices.SortFunc(m.providers, func(a, b shared.ProviderInfo) int {
		if providerKind(a) != providerKind(b) {
			if providerKind(a) == "Local" {
				return -1
			}
			return 1
		}
		if n := strings.Compare(a.Node, b.Node); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	items := make([]tui.Item, len(m.providers))
	selectedIndex := -1
	for i, p := range m.providers {
		items[i] = providerItem{p}
		if p.Name+"\x00"+p.Node == selected {
			selectedIndex = i
		}
	}
	m.list.SetItems(items)
	if selectedIndex >= 0 {
		m.list.SetCursor(selectedIndex)
	} else {
		m.list.SetCursor(0)
	}
}

func (m *ProvidersViewModel) updateProviderFilter(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.filterInput.Focused() {
		switch msg.String() {
		case "enter":
			m.filterInput.Blur()
		case "esc":
			m.filterInput.SetValue("")
			m.filterInput.Blur()
			m.filterProviders()
		default:
			var cmd tea.Cmd
			m.filterInput, cmd = m.filterInput.Update(msg)
			m.filterProviders()
			return true, cmd
		}
		return true, nil
	}
	switch msg.String() {
	case "/":
		return true, m.filterInput.Focus()
	case "tab":
		m.filterKind = (m.filterKind + 1) % 3
	case "x", "X":
		m.showDisabled = !m.showDisabled
	case "n", "N":
		nodes := []string{""}
		for _, p := range m.allProviders {
			if p.Node != "" && !slices.Contains(nodes, p.Node) {
				nodes = append(nodes, p.Node)
			}
		}
		slices.Sort(nodes)
		m.filterNode = nodes[(slices.Index(nodes, m.filterNode)+1)%len(nodes)]
	default:
		return false, nil
	}
	m.filterProviders()
	return true, nil
}

func (m *ProvidersViewModel) providerListStatus() string {
	if m.filterInput.Focused() {
		return "/ " + m.filterInput.View()
	}
	group := []string{"Local + cloud", "Local", "Cloud"}[m.filterKind]
	state := "enabled"
	if m.showDisabled {
		state = "including disabled"
	}
	node := m.filterNode
	if node == "" {
		node = "all nodes"
	}
	status := fmt.Sprintf("%d providers | %s | %s | %s", len(m.providers), group, node, state)
	if query := strings.TrimSpace(m.filterInput.Value()); query != "" {
		status = "Search: " + query + " | " + status
	}
	return status
}

func (m *ProvidersViewModel) updateUninstallConfirm(msg tea.KeyPressMsg) tea.Cmd {
	target := m.uninstallTarget
	if target == nil {
		return nil
	}
	switch tui.NewConfirm().UpdateKey(msg) {
	case tui.ConfirmNo, tui.ConfirmCancelled:
		m.uninstallTarget = nil
	case tui.ConfirmYes:
		m.uninstallTarget = nil
		if m.loadCancel != nil {
			m.loadCancel()
		}
		ctx, cancel := context.WithTimeout(m.Context(), constants.HTTPLongTimeout)
		m.uninstallContext, m.uninstallCancel = ctx, cancel
		m.actionStatus = fmt.Sprintf("Uninstalling %s from %s...", target.Name, target.Node)
		client, name, node := m.client, target.Name, target.Node
		return func() tea.Msg {
			return providersActionMsg{ctx: ctx, action: "uninstall", name: name, err: client.UninstallProviderContext(ctx, name, node)}
		}
	}
	return nil
}
