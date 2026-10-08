package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// ============================================================================
// Route detail view and form handlers (integrated into models view)
// ============================================================================

func (m *ModelsViewModel) viewRouteDetail(width, height int) string {
	g := m.selectedRouteGroup()
	if g == nil {
		return " No route selected\n"
	}

	strategy := g.Strategy
	if strategy == "" {
		strategy = pkgClient.StrategyPriority
	}

	d := shared.NewDetail(m.styles, width)
	if g.Description != "" {
		d.Field("Description", g.Description)
	}
	d.Field("Strategy", strategy)
	if desc, ok := strategyDescriptions[strategy]; ok {
		d.Text("               ", m.styles.Help.Render(desc))
	}

	if g.HealthCheck != nil {
		hcInfo := fmt.Sprintf("%s every %s (timeout %s)", g.HealthCheck.Path, g.HealthCheck.Interval, g.HealthCheck.Timeout)
		d.Field("Health Check", hcInfo)
	}

	d.Section(fmt.Sprintf("Deployments (%d)", len(g.Replicas)))

	footer := "\n" + shared.RenderViewFooter(m.styles, width, tui.ModelsKeys.RouteDetailHintsString())

	headerH := shared.RenderedHeight(d.String())
	footerH := shared.RenderedHeight(footer)
	availRows := max(height-headerH-footerH, 1)

	if len(g.Replicas) == 0 {
		d.Text(" ", m.styles.Help.Render("No deployments: this route is inactive. Press A to add one."))
	} else {
		depRows := make([][]string, len(g.Replicas))
		depStatuses := make([]string, len(g.Replicas))
		for i, d := range g.Replicas {
			status := replicaStatus(&d, m.NodeHealth)
			depStatuses[i] = status
			dot := statusDot(status)

			tags := shared.EmptyValue
			if len(d.Tags) > 0 {
				tags = strings.Join(d.Tags, ", ")
			}
			depRows[i] = []string{
				d.Name, d.Model, d.App,
				fmt.Sprintf("%d", d.Priority),
				tags, dot + " " + status,
			}
		}

		maxScroll := max(len(depRows)-availRows, 0)
		scroll := min(m.detail.Scroll(), maxScroll)
		end := min(scroll+availRows, len(depRows))

		rowStyles := make([]lipgloss.Style, end-scroll)
		for i, s := range depStatuses[scroll:end] {
			rowStyles[i] = shared.StatusStyle(m.styles, s)
		}

		dt := shared.NewScrollTable(shared.ScrollTableConfig{
			Headers:   []string{"NAME", "MODEL", "PROVIDER", "PRI", "TAGS", "STATUS"},
			Width:     width,
			Vis:       availRows,
			Cursor:    m.detailDepCursor,
			Offset:    scroll,
			Styles:    m.styles,
			StatusCol: 5,
			RowStyles: rowStyles,
			AllRows:   depRows,
		})

		for _, row := range depRows[scroll:end] {
			dt.Row(row...)
		}

		d.Write(dt.Render() + "\n")
	}

	if m.confirmKind == confirmDeleteDeployDetail {
		if m.detailDepCursor < len(g.Replicas) {
			dep := g.Replicas[m.detailDepCursor]
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Remove deployment %q? (y/n)", dep.Name)))
		}
	}

	d.Write(footer)
	return d.String()
}

func (m *ModelsViewModel) updateRouteDetail(msg tea.KeyPressMsg) tea.Cmd {
	g := m.selectedRouteGroup()

	switch {
	case key.Matches(msg, tui.ModelsKeys.Up):
		if m.detailDepCursor > 0 {
			m.detailDepCursor--
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.Down):
		if g != nil && m.detailDepCursor < len(g.Replicas)-1 {
			m.detailDepCursor++
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.Edit):
		if g != nil {
			m.routeForm.openEditGroupForm(g)
			m.mode = modelsViewEditRoute
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.AddDeploy):
		if g != nil {
			m.routeForm.openAddDeployForm(g)
			m.mode = modelsViewAddDeploy
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.Delete):
		if g != nil && isAutoRoute(g) {
			m.statusMsg = "Auto-route cannot be modified: model exists on multiple nodes"
			return nil
		}
		if g != nil && len(g.Replicas) > 0 && m.detailDepCursor < len(g.Replicas) {
			if len(g.Replicas) == 1 {
				m.confirmKind = confirmDeleteRoute
				m.confirmMessage = fmt.Sprintf("This is the last deployment. Remove it and delete route %q? (y/n)", g.Name)
			} else {
				dep := g.Replicas[m.detailDepCursor]
				m.confirmKind = confirmDeleteDeployDetail
				m.confirmMessage = fmt.Sprintf("Remove deployment %q? (y/n)", dep.Name)
			}
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.Back, tui.ModelsKeys.Enter):
		m.mode = modelsViewList
		m.detail.Reset()
		return nil
	case key.Matches(msg, tui.ModelsKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *ModelsViewModel) updateRouteForm(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		var existingGroup *pkgClient.ModelGroupResponse
		if m.mode == modelsViewEditRoute {
			existingGroup = m.selectedRouteGroup()
		}
		name, req := m.routeForm.buildGroupRequest(existingGroup)
		if req == nil {
			return nil // validation error set in statusMsg
		}
		// Prepend route prefix on creation (not edit)
		if m.mode == modelsViewNewRoute && m.routePrefix != "" && !strings.HasPrefix(name, m.routePrefix) {
			name = m.routePrefix + name
		}
		savingMode := m.mode
		oldName := m.routeForm.formEditName
		isRename := m.mode == modelsViewEditRoute && oldName != "" && name != oldName
		client := m.client
		return func() tea.Msg {
			group, err := client.CreateOrUpdateModelGroup(name, req)
			if err != nil {
				return routesSavedMsg{group: group, err: err, fromMode: savingMode}
			}
			// If renamed, delete the old group
			if isRename {
				if delErr := client.DeleteModelGroup(oldName); delErr != nil {
					return routesSavedMsg{group: group, err: fmt.Errorf("renamed but failed to delete old route %q: %w", oldName, delErr), fromMode: savingMode}
				}
			}
			return routesSavedMsg{group: group, err: nil, fromMode: savingMode}
		}

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		if m.mode == modelsViewEditRoute {
			m.mode = modelsViewRouteDetail
		} else {
			m.mode = modelsViewList
		}
		m.routeForm.cancel()
		return nil

	default:
		return m.routeForm.updateGroupForm(msg)
	}
}

func (m *ModelsViewModel) updateDeployFormMode(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		g := m.selectedRouteGroup()
		groupName, req := m.routeForm.buildDeployRequest(g)
		if req == nil {
			return nil // validation error
		}
		client := m.client
		return func() tea.Msg {
			group, err := client.CreateOrUpdateModelGroup(groupName, req)
			return routesSavedMsg{group: group, err: err, fromMode: modelsViewAddDeploy}
		}

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		m.mode = modelsViewRouteDetail
		m.routeForm.cancel()
		return nil

	default:
		return m.routeForm.updateDeployForm(msg)
	}
}

func (m *ModelsViewModel) viewRouteFormOverlay(width, height int, title string) string {
	bg := m.viewList(width, height)
	return renderRouteFormOverlay(&m.routeForm, m.styles, bg, width, height, title)
}

func (m *ModelsViewModel) viewDeployFormOverlay(width, height int) string {
	groupName := ""
	if g := m.selectedRouteGroup(); g != nil {
		groupName = g.Name
	}
	bg := m.viewRouteDetail(width, height)
	return renderDeployFormOverlay(&m.routeForm, m.styles, bg, width, height, groupName)
}

// removeSelectedDeployment removes the deployment at detailDepCursor from the selected route.
func (m *ModelsViewModel) removeSelectedDeployment() tea.Cmd {
	g := m.selectedRouteGroup()
	if g == nil || m.detailDepCursor >= len(g.Replicas) {
		m.confirmKind = confirmNone
		return nil
	}

	req := groupToRequest(g, m.detailDepCursor)
	m.confirmKind = confirmNone
	m.confirmMessage = ""
	client := m.client
	groupName := g.Name
	return func() tea.Msg {
		group, err := client.CreateOrUpdateModelGroup(groupName, req)
		return routesSavedMsg{group: group, err: err, fromMode: modelsViewRouteDetail}
	}
}

// removeDeploymentFromList removes a deployment from a route when triggered from the list view (child row).
func (m *ModelsViewModel) removeDeploymentFromList() tea.Cmd {
	r := m.selectedRow()
	if r == nil || !r.isChild() {
		m.confirmKind = confirmNone
		return nil
	}

	// Find the parent group
	var group *pkgClient.ModelGroupResponse
	for i := range m.groups {
		if m.groups[i].Name == r.routeName {
			group = &m.groups[i]
			break
		}
	}
	if group == nil {
		m.confirmKind = confirmNone
		return nil
	}

	// Find the deployment index
	depIdx := -1
	for i, d := range group.Replicas {
		if d.Name == r.deployment.Name {
			depIdx = i
			break
		}
	}
	if depIdx < 0 {
		m.confirmKind = confirmNone
		return nil
	}

	req := groupToRequest(group, depIdx)
	m.confirmKind = confirmNone
	m.confirmMessage = ""
	client := m.client
	groupName := group.Name
	return func() tea.Msg {
		group, err := client.CreateOrUpdateModelGroup(groupName, req)
		return routesSavedMsg{group: group, err: err, fromMode: modelsViewList}
	}
}
