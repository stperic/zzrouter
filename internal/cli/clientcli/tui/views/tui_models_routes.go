package views

import (
	"context"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
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

	hints := tui.ModelsKeys.RouteDetailHintsString() + "  ? help"
	status := m.statusMsg
	if m.confirmKind != confirmNone {
		status = m.confirmMessage
		hints = "Y remove  Enter/Esc cancel"
	}
	footer := shared.RenderViewFooterWithStatus(m.styles, width, status, hints)
	header := strings.TrimRight(d.String(), "\n")
	headerH := shared.RenderedHeight(header)
	if headerH > height-shared.RenderedHeight(footer)-shared.TuiTableHeaderAdd-1 {
		header = ""
		headerH = 0
	}
	availRows := max(height-headerH-shared.RenderedHeight(footer)-shared.TuiTableHeaderAdd, 1)
	if headerH > 0 {
		header += "\n"
	}
	var body strings.Builder
	body.WriteString(header)

	if len(g.Replicas) == 0 {
		body.WriteString(shared.TruncateText("No deployments. A adds a destination.", width) + "\n")
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
			node := d.Node
			if node == "" {
				node = m.localNode
			}
			depRows[i] = []string{
				d.Name, d.Model, d.App, node,
				fmt.Sprintf("%d", d.Priority),
				tags, dot + " " + status,
			}
		}

		m.detail.EnsureVisible(m.detailDepCursor, availRows)
		m.detail.SetVisible(availRows)
		maxScroll := max(len(depRows)-availRows, 0)
		scroll := min(m.detail.Scroll(), maxScroll)
		end := min(scroll+availRows, len(depRows))

		rowStyles := make([]lipgloss.Style, end-scroll)
		for i, s := range depStatuses[scroll:end] {
			rowStyles[i] = shared.StatusStyle(m.styles, s)
		}

		dt := shared.NewScrollTable(shared.ScrollTableConfig{
			Headers:        []string{"NAME", "MODEL", "PROVIDER", "NODE", "PRI", "TAGS", "STATUS"},
			ColumnPriority: []int{60, 100, 50, 90, 20, 10, 80},
			ColWidths:      []int{12, 14, 12, 12, 5, 8, 10},
			Width:          width,
			Vis:            availRows,
			Cursor:         m.detailDepCursor,
			Offset:         scroll,
			Styles:         m.styles,
			StatusCol:      6,
			RowStyles:      rowStyles,
			AllRows:        depRows,
		})

		for _, row := range depRows[scroll:end] {
			dt.Row(row...)
		}

		body.WriteString(dt.Render() + "\n")
	}

	body.WriteString(footer)
	return body.String()
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
			if existingGroup == nil {
				m.routeForm.statusMsg = "Route no longer exists. Your draft is preserved. Esc closes it."
				return nil
			}
		}
		name, req := m.routeForm.buildGroupRequest(existingGroup)
		if req == nil {
			return nil // validation error set in statusMsg
		}
		// Prepend route prefix on creation (not edit)
		if m.mode == modelsViewNewRoute && m.routePrefix != "" && !strings.HasPrefix(name, m.routePrefix) {
			name = m.routePrefix + name
		}
		oldName := ""
		if m.mode == modelsViewEditRoute && m.routeForm.formEditName != name {
			oldName = m.routeForm.formEditName
		}
		return m.saveRoute(name, req, m.mode, oldName)

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return func() tea.Msg { return routeCloseMsg{owner: m} }

	default:
		before := m.routeForm.values()
		cmd := m.routeForm.updateGroupForm(msg)
		if before != m.routeForm.values() {
			m.Dirty = true
			m.Draft.Discard = m.discardRouteForm
		}
		return cmd
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
		return m.saveRoute(groupName, req, modelsViewAddDeploy, "")

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return func() tea.Msg { return routeCloseMsg{owner: m} }

	default:
		before := m.routeForm.values()
		cmd := m.routeForm.updateDeployForm(msg)
		if before != m.routeForm.values() {
			m.Dirty = true
			m.Draft.Discard = m.discardRouteForm
		}
		return cmd
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
	return m.saveRoute(g.Name, req, modelsViewRouteDetail, "")
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
	return m.saveRoute(group.Name, req, modelsViewList, "")
}

type routeCloseMsg struct{ owner *ModelsViewModel }

func (m *ModelsViewModel) discardRouteForm() {
	switch m.mode {
	case modelsViewEditRoute, modelsViewAddDeploy:
		m.mode = modelsViewRouteDetail
		if m.selectedRouteGroup() == nil {
			m.mode = modelsViewList
			m.activeRoute = ""
		}
	case modelsViewNewRoute:
		m.mode = modelsViewList
	default:
		return
	}
	m.routeForm.cancel()
	m.Dirty = false
}

// NavigationBlocked keeps route form state stable while its write completes.
func (m *ModelsViewModel) NavigationBlocked() bool { return m.routeWriteContext != nil }

func (m *ModelsViewModel) beginRouteWrite() context.Context {
	if m.loadCancel != nil {
		m.loadCancel()
	}
	m.fetching = false
	m.routeWriteContext, m.routeWriteCancel = context.WithTimeout(m.Context(), constants.HTTPLongTimeout)
	m.routeForm.statusMsg = "Saving route. Please wait..."
	m.statusMsg = m.routeForm.statusMsg
	return m.routeWriteContext
}

func (m *ModelsViewModel) finishRouteWrite(ctx context.Context) bool {
	if ctx == nil || ctx != m.routeWriteContext || m.Context().Err() != nil {
		return false
	}
	m.routeWriteCancel()
	m.routeWriteContext = nil
	return true
}

func (m *ModelsViewModel) saveRoute(name string, req *pkgClient.CreateOrUpdateModelGroupRequest, mode modelsViewMode, oldName string) tea.Cmd {
	if m.NavigationBlocked() || m.routeWriteUncertain {
		return nil
	}
	ctx, client := m.beginRouteWrite(), m.client
	return func() tea.Msg {
		group, err := client.CreateOrUpdateModelGroupContext(ctx, name, req)
		if err == nil && oldName != "" {
			if deleteErr := client.DeleteModelGroupContext(ctx, oldName); deleteErr != nil {
				err = fmt.Errorf("saved %q but could not remove old route %q: %w", name, oldName, deleteErr)
			}
		}
		return routesSavedMsg{ctx: ctx, group: group, err: err, fromMode: mode}
	}
}

// Cancel releases reads and bounded route writes when the host shuts down.
func (m *ModelsViewModel) Cancel() {
	m.ViewContext.Cancel()
	if m.loadCancel != nil {
		m.loadCancel()
	}
	if m.usageCancel != nil {
		m.usageCancel()
	}
	if m.routeWriteCancel != nil {
		m.routeWriteCancel()
	}
}
