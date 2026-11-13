package views

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

type providersInstallComponentsMsg struct {
	data *pkgClient.ProviderEnvironmentResponse
	err  error
	ctx  context.Context
}

func (m *ProvidersViewModel) openInstallComponents() {
	if m.installPlan == nil || m.installPlan.ResolvedInstall == nil {
		m.actionStatus = "This runtime has no optional dependencies."
		return
	}
	m.showInstallComponents(m.installPlan.ResolvedInstall, m.installPlan.Force)
}

func (m *ProvidersViewModel) showInstallComponents(recipe *pkgClient.InstallRecipeResponse, force bool) {
	labels := make(map[string]string, len(recipe.Recipe.Components))
	for id, component := range recipe.Recipe.Components {
		labels[id] = id + ": " + component.Description
	}
	for _, id := range recipe.SelectedComponents {
		if _, ok := labels[id]; !ok {
			labels[id] = id + ": Unavailable in this release; clear this selection to continue."
		}
	}
	if len(labels) == 0 {
		m.actionStatus = "This runtime has no optional dependencies."
		return
	}
	m.installComponentRecipe, m.installComponentForce = recipe, force
	var options, selected []string
	ids := make([]string, 0, len(labels))
	for id := range labels {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		label := labels[id]
		options = append(options, label)
		if slices.Contains(recipe.SelectedComponents, id) {
			selected = append(selected, label)
		}
	}
	m.installComponents = form.NewChecklist("Dependencies", options, "None available", "Base dependencies only")
	m.installComponents.SetSelected(selected)
	m.installComponents.Focus()
	m.resizeInstallComponents()
	m.actionStatus = ""
}

func (m *ProvidersViewModel) updateInstallComponents(msg tea.KeyPressMsg) tea.Cmd {
	if m.termHeight > 0 && m.termHeight < 18 && msg.String() != "esc" {
		return nil
	}
	if handled, cmd := m.installComponents.HandleKey(msg); handled {
		return cmd
	}
	switch msg.String() {
	case "esc":
		m.installComponents = nil
		if m.installPlan == nil {
			m.retireInstallRequest()
			m.mode = providersViewList
		}
	case "enter":
		selected := make([]string, 0)
		for _, label := range m.installComponents.Selected() {
			id, _, _ := strings.Cut(label, ": ")
			selected = append(selected, id)
		}
		options := pkgClient.InstallPlanOptions{Components: &selected, Force: m.installComponentForce || !slices.Equal(selected, m.installComponentRecipe.SelectedComponents)}
		m.installComponents = nil
		return m.fetchInstallPlan(options)
	}
	return nil
}

func (m *ProvidersViewModel) viewInstallComponents(width, height int) string {
	if height < 18 {
		return "Resize the terminal to choose dependencies.\nEsc: cancel."
	}
	text := " Optional dependencies\n\n" + m.installComponents.View(width, m.styles) + "\nChanging the selection rebuilds the managed environment.\nReview the new plan before running it.\nEnter: preview selection. Esc: cancel.\n"
	lines := strings.Split(text, "\n")
	for index := range lines {
		lines[index] = shared.TruncateText(lines[index], max(1, width-2))
	}
	return strings.Join(lines, "\n")
}

func (m *ProvidersViewModel) resizeInstallComponents() {
	if m.installComponents != nil {
		m.installComponents.SetViewportSize(min(8, max(1, m.termHeight-17)))
	}
}

func (m *ProvidersViewModel) openSelectedProviderComponents() tea.Cmd {
	provider := m.selectedProvider()
	if provider == nil {
		return nil
	}
	if provider.Mode != constants.AppModeOnDemand {
		m.actionStatus = "This provider has no optional managed dependencies."
		return nil
	}
	m.installProvider, m.installNode = provider.Name, provider.Node
	m.installComponents = nil
	m.actionStatus = ""
	ctx := m.beginInstallRequest()
	m.installLoading = true
	m.installPlan = nil
	m.mode = providersViewInstallSteps
	client, name, node := m.client, m.installProvider, m.installNode
	return func() tea.Msg {
		data, err := client.GetProviderEnvironmentContext(ctx, name, node)
		return providersInstallComponentsMsg{data: data, err: err, ctx: ctx}
	}
}

func (m *ProvidersViewModel) applyInstallComponents(msg providersInstallComponentsMsg) {
	if m.mode != providersViewInstallSteps || !m.installLoading || !m.ownsInstallRequest(msg.ctx) {
		return
	}
	m.installLoading = false
	if msg.err != nil {
		m.actionStatus = fmt.Sprintf("Failed to read optional dependencies: %v", msg.err)
		m.mode = providersViewList
		return
	}
	if msg.data.DesiredInstall == nil {
		m.actionStatus = "This runtime has no optional dependencies."
		m.mode = providersViewList
		return
	}
	recipe := &pkgClient.InstallRecipeResponse{}
	recipe.Recipe.Components = msg.data.DesiredInstall.Recipe.Components
	if msg.data.InstalledManifest != nil {
		recipe.SelectedComponents = slices.Clone(msg.data.InstalledManifest.SelectedComponents)
	}
	m.showInstallComponents(recipe, true)
	if m.installComponents == nil {
		m.mode = providersViewList
	}
}
