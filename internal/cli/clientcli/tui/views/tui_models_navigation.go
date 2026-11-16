package views

import (
	"fmt"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
)

func (m *ModelsViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	if m.list.UpdateKey(msg) {
		m.statusMsg = ""
		m.operationStatus = ""
		return nil
	}

	r := m.selectedRow()

	switch {
	case key.Matches(msg, tui.ModelsKeys.Enter):
		if r == nil {
			return nil
		}
		if r.isRoute() {
			m.activeRoute = r.RawModel
			m.mode = modelsViewRouteDetail
			m.detailDepCursor = 0
			return nil
		}
		// Both a standalone row and a child deployment row open model detail.
		m.mode = modelsViewDetail
		m.modelUsage, m.modelUsageIdle = nil, false
		return m.fetchModelUsage(r.RawModel, usageProviderFor(r))

	// Route expand/collapse
	case key.Matches(msg, tui.ModelsKeys.Space):
		if r != nil && r.isRoute() {
			m.expandedRoutes[r.RawModel] = !m.expandedRoutes[r.RawModel]
			m.rebuildDisplayRows()
		}
		return nil

	case key.Matches(msg, tui.ModelsKeys.Right):
		if r != nil && r.isRoute() && !m.expandedRoutes[r.RawModel] {
			m.expandedRoutes[r.RawModel] = true
			m.rebuildDisplayRows()
		}
		return nil

	case key.Matches(msg, tui.ModelsKeys.Left):
		if r != nil && r.isRoute() && m.expandedRoutes[r.RawModel] {
			m.expandedRoutes[r.RawModel] = false
			m.rebuildDisplayRows()
		}
		return nil

	// New route
	case key.Matches(msg, tui.ModelsKeys.New):
		m.routeForm.openNewGroupForm()
		m.mode = modelsViewNewRoute
		return nil

	case key.Matches(msg, tui.ModelsKeys.Sort):
		var selected rowID
		if r != nil {
			selected = r.id()
		}
		// Cycle through sort options
		current := m.sortBy
		if current == "" {
			current = "status"
		}
		for i, opt := range modelsSortOptions {
			if opt == current {
				m.sortBy = modelsSortOptions[(i+1)%len(modelsSortOptions)]
				break
			}
		}
		m.applySortBy()
		m.syncListItems()
		m.restoreCursor(selected, r != nil)
		return nil

	case key.Matches(msg, tui.ModelsKeys.Refresh):
		m.loading = true
		m.fetching = true
		return m.fetchAll()

	case key.Matches(msg, tui.ModelsKeys.Delete):
		if r != nil && r.isRoute() {
			if isAutoRoute(r.route) {
				m.statusMsg = "Auto-route cannot be deleted: model exists on multiple nodes"
				return nil
			}
			m.confirmKind = confirmDeleteRoute
			m.confirmMessage = fmt.Sprintf("Delete route %q? (y/n)", r.RawModel)
			return nil
		}
		if r != nil && r.isChild() {
			// Check if parent is auto-route
			var parentGroup *pkgClient.ModelGroupResponse
			for i := range m.groups {
				if m.groups[i].Name == r.routeName {
					parentGroup = &m.groups[i]
					break
				}
			}
			if parentGroup != nil && isAutoRoute(parentGroup) {
				m.statusMsg = "Auto-route cannot be modified: model exists on multiple nodes"
				return nil
			}
			if parentGroup != nil && len(parentGroup.Replicas) == 1 {
				m.confirmKind = confirmDeleteRoute
				m.confirmMessage = fmt.Sprintf("This is the last deployment. Remove it and delete route %q? (y/n)", r.routeName)
			} else {
				m.confirmKind = confirmDeleteDeployList
				m.confirmMessage = fmt.Sprintf("Remove deployment %q from %q? (y/n)", r.deployment.Name, r.routeName)
			}
			return nil
		}
		return nil

	case key.Matches(msg, tui.ModelsKeys.Back):
		return tui.NavBack()

	case key.Matches(msg, tui.ModelsKeys.Quit):
		return tea.Quit
	}

	return nil
}

func (m *ModelsViewModel) updateDetail(msg tea.KeyPressMsg) tea.Cmd {
	if m.detail.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.ModelsKeys.Edit):
		if r := m.selectedRow(); r != nil && r.Provider != "" && r.Provider != shared.EmptyValue {
			provider := r.Provider
			model, node := r.RawModel, r.Node
			return func() tea.Msg {
				return tui.OpenParamsEditorMsg{Provider: provider, Model: model, Node: node}
			}
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.Chat):
		r := m.selectedRow()
		if r == nil || r.deploy != nil || r.RawModel == "" {
			return nil
		}
		model, node := r.RawModel, r.Node
		if node == constants.CloudNodeName {
			node = ""
		}
		return func() tea.Msg { return tui.LaunchChatMsg{Model: model, Node: node} }
	case key.Matches(msg, tui.ModelsKeys.Test):
		if m.ping.inFlight() {
			return nil
		}
		return m.actionPing()
	case key.Matches(msg, tui.ModelsKeys.StartStop):
		r := m.selectedRow()
		if r == nil || r.deploy != nil || r.IsCloud {
			return nil
		}
		return m.actionStartStop()
	case key.Matches(msg, tui.ModelsKeys.Delete):
		return m.actionDelete()
	case key.Matches(msg, tui.ModelsKeys.Back, tui.ModelsKeys.Enter):
		m.mode = modelsViewList
		if m.usageCancel != nil {
			m.usageCancel()
		}
		m.detail.Reset()
		return nil
	case key.Matches(msg, tui.ModelsKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *ModelsViewModel) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	if m.NavigationBlocked() || m.routeWriteUncertain {
		m.confirmKind = confirmNone
		m.statusMsg = "Refresh routes before another write."
		return nil
	}
	switch {
	case key.Matches(msg, tui.DefaultConfirmKeyBindings.Yes):
		// Handle route-specific confirmations
		if m.confirmKind == confirmDeleteRoute {
			// Determine route name from context (route row, child row, or detail view)
			var routeName string
			if m.targetsRoute() {
				if g := m.selectedRouteGroup(); g != nil {
					routeName = g.Name
				}
			} else if r := m.selectedRow(); r != nil {
				if r.isRoute() {
					routeName = r.RawModel
				} else if r.isChild() {
					routeName = r.routeName
				}
			}
			if routeName == "" {
				if g := m.selectedRouteGroup(); g != nil {
					routeName = g.Name
				}
			}
			if routeName != "" {
				client := m.client
				ctx := m.beginRouteWrite()
				m.confirmKind = confirmNone
				m.confirmMessage = ""
				return func() tea.Msg {
					err := client.DeleteModelGroupContext(ctx, routeName)
					return routesDeletedMsg{ctx: ctx, err: err}
				}
			}
			m.confirmKind = confirmNone
			return nil
		}
		if m.confirmKind == confirmDeleteDeployList {
			return m.removeDeploymentFromList()
		}
		// Route form confirmation (from route detail view)
		if m.confirmKind == confirmDeleteDeployDetail {
			return m.removeSelectedDeployment()
		}
		return m.executeConfirmedAction()
	case key.Matches(msg, tui.DefaultConfirmKeyBindings.No, tui.DefaultConfirmKeyBindings.Cancel):
		m.confirmKind = confirmNone
		m.confirmMessage = ""
		return nil
	}
	return nil
}

func (m *ModelsViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		if m.mode == modelsViewDetail {
			m.mode = modelsViewList
			if m.usageCancel != nil {
				m.usageCancel()
			}
			m.detail.Reset()
			m.confirmKind = confirmNone
			return nil
		}
		return tui.NavBack()
	}
	if m.mode != modelsViewList || m.confirmKind != confirmNone {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if !ok {
		return nil
	}
	if enter {
		m.mode = modelsViewDetail
	} else {
		m.statusMsg = ""
	}
	return nil
}

func (m *ModelsViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	switch m.mode {
	case modelsViewDetail:
		m.detail.UpdateWheel(msg)
	case modelsViewList:
		m.list.UpdateWheel(msg)
	}
}

func (m *ModelsViewModel) Breadcrumb() []string {
	base := []string{"Models", m.listLabel()}
	switch m.mode {
	case modelsViewDetail:
		if r := m.selectedRow(); r != nil {
			label := r.RawModel
			if label == "" {
				label = r.Provider
			}
			return append(base, label)
		}
		return append(base, "Details")
	case modelsViewRouteDetail:
		if g := m.selectedRouteGroup(); g != nil {
			return append(base, g.Name)
		}
		return append(base, "Route")
	case modelsViewNewRoute:
		return append(base, "New Route")
	case modelsViewEditRoute:
		return append(base, "Edit Route")
	case modelsViewAddDeploy:
		return append(base, "Add Deployment")
	default:
		return base
	}
}

// View implements tea.Model.
func (m *ModelsViewModel) View() tea.View {
	hints := modelsListHints(m.selectedRow())
	if m.mode == modelsViewRouteDetail {
		hints = tui.ModelsKeys.RouteDetailHintsString()
	}
	if m.mode == modelsViewDetail {
		hints = modelsDetailHints(m.selectedRow())
	}
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.Draft.View(m.styles, m.help.View(m.styles, m.viewContent(), m.termWidth, m.termHeight, hints), m.termWidth, m.termHeight))
}

func (m *ModelsViewModel) viewContent() string {
	switch m.mode {
	case modelsViewDetail:
		return m.viewDetail(m.termWidth, m.termHeight)
	case modelsViewRouteDetail:
		return m.viewRouteDetail(m.termWidth, m.termHeight)
	case modelsViewNewRoute:
		return m.viewRouteFormOverlay(m.termWidth, m.termHeight, "New Route")
	case modelsViewEditRoute:
		return m.viewRouteFormOverlay(m.termWidth, m.termHeight, "Edit Route")
	case modelsViewAddDeploy:
		return m.viewDeployFormOverlay(m.termWidth, m.termHeight)
	default:
		return m.viewList(m.termWidth, m.termHeight)
	}
}
