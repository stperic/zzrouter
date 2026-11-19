package views

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func assertFrame(t *testing.T, frame string, width, height int) string {
	t.Helper()
	require.LessOrEqual(t, lipgloss.Width(frame), width, "frame width")
	require.LessOrEqual(t, lipgloss.Height(frame), height, "frame height: %s", ansi.Strip(frame))
	return ansi.Strip(frame)
}

func TestResourceListsFitResizeAndKeepStatus(t *testing.T) {
	styles := ui.NewStyles(ui.ClaudeCodeTheme())
	nodes := NewNodesViewModel(nil, styles)
	t.Cleanup(nodes.Cancel)
	nodes.Update(nodesLoadedMsg{nodes: []shared.NodeInfo{{Name: "studio", HealthStatus: "healthy", Memory: "64 / 128 GiB", GPU: "24 GiB"}, {Name: "worker", HealthStatus: "down"}}})
	providers := providerListFixture(t)
	models := NewModelsViewModel(nil, styles)
	t.Cleanup(models.Cancel)
	loadModelRows(models, []modelRow{instanceRow("qwen", "a", "running"), instanceRow("llama", "b", "running")}, nil)
	for name, view := range map[string]tea.Model{"nodes": nodes, "providers": providers, "models": models} {
		t.Run(name, func(t *testing.T) {
			for _, size := range [][2]int{{80, 24}, {40, 10}, {120, 32}, {40, 10}} {
				view.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				frame := assertFrame(t, view.View().Content, size[0], size[1])
				require.Contains(t, frame, "STATUS")
				require.Contains(t, frame, "Esc")
				require.Contains(t, frame, "? help")
			}
		})
	}
}

func TestFormsKeepLastFocusedFieldAndActionsAtFortyByTen(t *testing.T) {
	styles := ui.NewStyles(ui.ClaudeCodeTheme())
	pricing := newPricingView(t, nil, nil)
	pricing.openForm(pkgClient.PricingOverride{Model: "qwen"}, false)
	pricing.formFocus = pricingFieldNote
	pricing.setFormFocus()
	pricing.formNote.SetValue("pricing-note")
	pricing.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	frame := assertFrame(t, pricing.View().Content, 40, 10)
	require.Contains(t, frame, "Note:")
	require.Contains(t, frame, "Ctrl+S save")
	require.Contains(t, frame, "Esc cancel")

	keys := NewVkeysViewModel(nil, styles)
	t.Cleanup(keys.Cancel)
	keys.openNewKeyForm()
	keys.formFocus = vkeysFieldExpires
	keys.setKeyFormFocus()
	keys.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	frame = assertFrame(t, keys.View().Content, 40, 10)
	require.Contains(t, frame, "Expires:")
	require.Contains(t, frame, "Ctrl+S save")
	require.Contains(t, frame, "Esc cancel")

	routes := NewModelsViewModel(nil, styles)
	t.Cleanup(routes.Cancel)
	routes.mode = modelsViewNewRoute
	routes.routeForm.openNewGroupForm()
	routes.routeForm.formFocus = routeFieldStrategy
	routes.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	frame = assertFrame(t, routes.View().Content, 40, 10)
	require.Contains(t, frame, "Strategy:")
	require.Contains(t, frame, "Ctrl+S save")
	require.Contains(t, frame, "Esc cancel")
}

func TestRouteDestinationsFitAndSelectionRemainsVisible(t *testing.T) {
	m := NewModelsViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(m.Cancel)
	g := pkgClient.ModelGroupResponse{Name: "chat", Description: "A long description of the route for small terminal tests"}
	for i := 0; i < 10; i++ {
		g.Replicas = append(g.Replicas, pkgClient.ReplicaResponse{Name: fmt.Sprint(i), Model: fmt.Sprintf("qwen-%d", i), Node: "studio", App: "ollama"})
	}
	loadModelRows(m, nil, []pkgClient.ModelGroupResponse{g})
	m.activeRoute = "chat"
	m.mode = modelsViewRouteDetail
	m.NodeHealth = map[string]string{"studio": "healthy"}
	for _, size := range [][2]int{{80, 24}, {40, 10}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.detailDepCursor = 9
		frame := assertFrame(t, m.View().Content, size[0], size[1])
		require.Contains(t, frame, "qwen-9")
		require.Contains(t, frame, "studio")
		require.Contains(t, frame, "STATUS")
		require.Contains(t, frame, "C chat")
	}
	_, openChat := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	require.NotNil(t, openChat)
	require.Equal(t, tui.LaunchChatMsg{Model: g.Name}, openChat())
}

func TestDraftPromptFitsAndRetainsKeepAction(t *testing.T) {
	d := tui.Draft{Dirty: true}
	d.ConfirmNavigation(tui.NavBackMsg{})
	frame := assertFrame(t, d.View(ui.NewStyles(ui.ClaudeCodeTheme()), "", 40, 6), 40, 6)
	require.Contains(t, frame, "Unsaved changes")
	require.Contains(t, frame, "Y discard")
	require.Contains(t, frame, "keep editing")
}

func TestPricingAndKeyConfirmationsFitShortTerminal(t *testing.T) {
	pricing := newPricingView(t, []pkgClient.PricingOverride{{Model: "qwen", Provider: "ollama"}}, map[string]int64{"ollama/qwen": 4})
	pricing.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	pricing.statusMsg = "An API response with a long explanation should not obscure the footer"
	assertFrame(t, pricing.View().Content, 40, 10)
	pricing.confirmPending = true
	frame := assertFrame(t, pricing.View().Content, 40, 10)
	require.Contains(t, frame, "Y delete")
	pricing.confirmPending = false
	pricing.mode = pricingViewUnpriced
	assertFrame(t, pricing.View().Content, 40, 10)
	keys := NewVkeysViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(keys.Cancel)
	keys.loading = false
	keys.keys = []*pkgClient.KeyResponse{{ID: "test-key", Name: "Coding", Role: "user"}}
	keys.list.SetItems([]tui.Item{vkeyItem{keys.keys[0]}})
	keys.confirmPending = true
	keys.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	frame = assertFrame(t, keys.View().Content, 40, 10)
	require.Contains(t, frame, "test-key")
	require.Contains(t, frame, "STATUS")
	require.Contains(t, frame, "Y delete")
}

func TestProviderDetailsScrollAsOnePane(t *testing.T) {
	m := providerListFixture(t)
	m.mode = providersViewDetail
	m.detailParams = []pkgClient.ParameterEntry{{Key: "context-size", Value: "32768"}, {Key: "last-setting", Value: "visible-at-end"}}
	for _, size := range [][2]int{{80, 24}, {40, 10}} {
		m.detail.Reset()
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		frame := assertFrame(t, m.View().Content, size[0], size[1])
		require.Contains(t, frame, "E parameters")
		require.Contains(t, frame, "Esc back")
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
		frame = assertFrame(t, m.View().Content, size[0], size[1])
		require.Contains(t, frame, "visible-at-end")
		require.Contains(t, frame, "Esc back")
	}
}
