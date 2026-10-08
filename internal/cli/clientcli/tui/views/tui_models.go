package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// modelRowItem wraps modelRow to satisfy tui.Item.
type modelRowItem struct{ row *modelRow }

func (i modelRowItem) Title() string       { return i.row.Model }
func (i modelRowItem) Description() string { return i.row.Status }
func (i modelRowItem) FilterValue() string {
	return i.row.Model + " " + i.row.Provider + " " + i.row.Node
}

// rowKind discriminates the type of entry a modelRow represents.
type rowKind int

const (
	rowKindModel       rowKind = iota // standalone model (registry, instance, or download)
	rowKindRoute                      // route header (expandable folder)
	rowKindDeployChild                // deployment row under a route
)

// modelRow is a unified row combining registry models, running instances, downloads, and routes.
type modelRow struct {
	Kind       rowKind
	Node       string
	Model      string // formatted display name with emoji/icon
	RawModel   string // raw model name for API calls
	Provider   string // inference app (vllm, ollama, cloudflare) — used for actions
	SourceRepo string // source registry (ollama, huggingface, cloudflare) — from API
	Size       string
	Status     string

	// Metadata flags
	IsCloud bool // true = cloud provider model (no start/stop, no local download)

	// Sort priority: -1 = route, 0 = downloading, 1 = running, 2 = idle
	sortPriority int

	// Source data for detail view and actions (model rows)
	registry *pkgClient.ModelMetadata
	instance *pkgClient.Instance
	deploy   *shared.DeploymentInfo

	// Route fields (route and deploy-child rows)
	route      *pkgClient.ModelGroupResponse // route header payload
	routeName  string                        // parent route name (for children)
	deployment *pkgClient.ReplicaResponse    // replica data (for children)
}

func (r *modelRow) isRoute() bool { return r.Kind == rowKindRoute }
func (r *modelRow) isChild() bool { return r.Kind == rowKindDeployChild }

// modelsViewMode tracks what the view is showing
type modelsViewMode int

const (
	modelsViewList        modelsViewMode = iota // Table list
	modelsViewDetail                            // Detail panel for selected model
	modelsViewRouteDetail                       // Detail panel for selected route
	modelsViewNewRoute                          // Overlay: create new route
	modelsViewEditRoute                         // Overlay: edit route settings
	modelsViewAddDeploy                         // Overlay: add deployment to route
)

type modelsLoadedMsg struct {
	rows        []modelRow
	groups      []pkgClient.ModelGroupResponse
	routePrefix string
	err         error
	warnings    []string // non-fatal errors (e.g. groups fetch failed)
}

type modelsTickMsg time.Time

// modelsAutoRefreshMsg carries the epoch of the poll chain that produced it.
// A chain abandoned while a child view was on top can still deliver one late
// message; the epoch lets the handler drop it instead of re-arming a second
// chain alongside the one Refresh started.
type modelsAutoRefreshMsg struct{ epoch int }

// deploymentsRefreshedMsg is sent when only the pulls data is refreshed (auto-refresh tick)
type deploymentsRefreshedMsg struct {
	pulls []shared.DeploymentInfo
	err   error
}

// modelsActionMsg is returned after an async action (start/stop/delete) completes
type modelsActionMsg struct {
	action string // "start", "stop", "delete"
	err    error
}

// modelConfirmKind identifies the pending confirmation action.
type modelConfirmKind int

const (
	confirmNone modelConfirmKind = iota
	confirmStopModel
	confirmDeleteModel
	confirmDeleteDownload
	confirmDeleteRoute
	confirmDeleteDeployList   // remove deployment from list view (child row)
	confirmDeleteDeployDetail // remove deployment from route detail view
)

// modelsSortOptions defines the available sort options and their cycle order.
var modelsSortOptions = []string{"status", "model", "app", "node"}

type ModelsViewModel struct {
	tui.ViewContext

	client     *pkgClient.Client
	styles     ui.Styles
	rows       []modelRow
	list       tui.List
	detail     tui.Detail
	termWidth  int
	termHeight int
	loading    bool
	err        error
	hasActive  bool   // true if any downloads are in progress
	fetching   bool   // true while a fetch is in flight
	ticking    bool   // true while a pull-refresh tick timer is running
	sortBy     string // current sort column (default: "status")

	// View mode
	mode modelsViewMode

	// Confirmation dialog
	confirmKind    modelConfirmKind
	confirmMessage string

	// Detail view — running usage for the selected model since node start.
	modelUsage *pkgClient.ModelUsageResponse
	// modelUsageIdle marks a model the node has served nothing for, which is
	// reported rather than left blank.
	modelUsageIdle bool

	// Status message (shown briefly after actions)
	statusMsg string

	// Ping slot for the detail panel's Test section
	ping pingState

	// Loading spinner tick
	loadTick  int
	spinning  bool // a SpinnerTickCmd chain is live; guards against arming a second
	pollEpoch int  // bumped on re-activation to retire the previous poll chain

	// Route integration
	expandedRoutes map[string]bool                // route name → expanded
	NodeHealth     map[string]string              // node name → "online"/"offline"
	groups         []pkgClient.ModelGroupResponse // cached groups for route operations
	routePrefix    string                         // prefix for route names (from server config)

	// Route detail .View: selected deployment within the route detail panel.
	// This is a sub-list cursor distinct from the main list cursor — small
	// fixed list, no paging, kept as raw int.
	detailDepCursor int

	// Route form overlay
	routeForm routeFormState

	// After creating a new route, navigate to its detail
	pendingRouteName string

	// Raw model rows before route insertion (for rebuilding display on expand/collapse)
	rawModelRows []modelRow
}

func NewModelsViewModel(client *pkgClient.Client, styles ui.Styles) *ModelsViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up:       tui.ModelsKeys.Up,
		Down:     tui.ModelsKeys.Down,
		PageUp:   tui.ModelsKeys.PageUp,
		PageDown: tui.ModelsKeys.PageDown,
		Home:     tui.ModelsKeys.Home,
		End:      tui.ModelsKeys.End,
	})
	detail := tui.NewDetail()
	detail.SetKeys(tui.NavigationKeys{
		Up:       tui.ModelsKeys.Up,
		Down:     tui.ModelsKeys.Down,
		PageUp:   tui.ModelsKeys.PageUp,
		PageDown: tui.ModelsKeys.PageDown,
		Home:     tui.ModelsKeys.Home,
		End:      tui.ModelsKeys.End,
	})
	return &ModelsViewModel{
		client:         client,
		styles:         styles,
		loading:        true,
		expandedRoutes: make(map[string]bool),
		routeForm:      newRouteFormState(),
		list:           list,
		detail:         detail,
	}
}

// syncListItems builds tui.Item entries from m.rows. Call after any change
// to m.rows (data load, sort, expand/collapse, in-place mutation).
func (m *ModelsViewModel) syncListItems() {
	items := make([]tui.Item, len(m.rows))
	for i := range m.rows {
		items[i] = modelRowItem{row: &m.rows[i]}
	}
	m.list.SetItems(items)
}

// Init implements tea.Model.
func (m *ModelsViewModel) Init() tea.Cmd {
	m.fetching = true
	// Use refresh=true on init to ensure server cache is populated and auto-routes are created
	m.spinning = true
	return tea.Batch(m.fetchAll(), shared.SpinnerTickCmd(), m.autoRefreshCmd())
}

// Update implements tea.Model.
func (m *ModelsViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
		m.list.SetVisible(vis)
		m.detail.SetVisible(vis)
		return m, nil

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		if msg.Level == 1 {
			m.mode = modelsViewList
			m.confirmKind = confirmNone
		}
		return m, nil

	case tea.MouseClickMsg:
		return m, m.handleMouseClick(msg)

	case tea.MouseWheelMsg:
		m.handleMouseWheel(msg)
		return m, nil

	case shared.SpinnerTickMsg:
		// fetching too: a silent reload's only signal is the footer spinner.
		// ping too: its only signal is the Test line in the detail panel.
		if m.loading || m.fetching || m.ping.inFlight() {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		m.spinning = false
		return m, nil

	case modelsLoadedMsg:
		m.loading = false
		m.fetching = false
		// A failed background reload must not swap populated content for an
		// error page: keep what is on screen and report in the status line.
		if msg.err != nil && len(m.rows) > 0 {
			m.statusMsg = fmt.Sprintf("Refresh failed: %v", msg.err)
			return m, nil
		}
		// The reload re-sorts by status, so a model that just started moves.
		var selected rowID
		r := m.selectedRow()
		hadSelection := r != nil
		if hadSelection {
			selected = r.id()
		}
		if msg.groups != nil {
			m.groups = msg.groups
		}
		if msg.routePrefix != "" {
			m.routePrefix = msg.routePrefix
		}
		m.err = msg.err
		if len(msg.warnings) > 0 {
			m.statusMsg = strings.Join(msg.warnings, "; ")
		}
		m.rows = m.buildDisplayRows(msg.rows, m.groups)
		m.applySortBy()
		m.syncListItems()
		m.restoreCursor(selected, hadSelection)
		// Populate picker from fetched registry models (avoids redundant API call)
		if !m.routeForm.pickerLoaded && len(msg.rows) > 0 {
			var pickerModels []pkgClient.ModelMetadata
			for _, r := range msg.rows {
				if r.registry != nil {
					pickerModels = append(pickerModels, *r.registry)
				}
			}
			m.routeForm.availableModels = pickerModels
			m.routeForm.pickerLoaded = true
		}
		m.hasActive = false
		for _, r := range m.rows {
			if r.sortPriority == 0 {
				m.hasActive = true
				break
			}
		}
		// Handle pending navigation to route detail after creation
		if m.pendingRouteName != "" {
			for i, r := range m.rows {
				if r.isRoute() && r.route.Name == m.pendingRouteName {
					m.list.SetCursor(i)
					m.mode = modelsViewRouteDetail
					m.detailDepCursor = 0
					break
				}
			}
			m.pendingRouteName = ""
		}
		if m.hasActive && !m.ticking {
			m.ticking = true
			return m, m.tickCmd()
		}
		return m, nil

	case modelUsageMsg:
		// A transport failure is not surfaced: the panel is supplementary and
		// an error banner over the model detail would be noise. An idle model
		// is different — it gets an explicit "0 since restart".
		m.modelUsageIdle = msg.idle
		if msg.err == nil {
			m.modelUsage = msg.usage
		}
		return m, nil

	case modelsAutoRefreshMsg:
		if msg.epoch != m.pollEpoch {
			return m, nil // retired chain
		}
		// Skip groups: static config, only changes on user action.
		if m.loading || m.fetching {
			return m, m.autoRefreshCmd()
		}
		return m, tea.Batch(m.fetchAllWithGroups(true, false), m.autoRefreshCmd())

	case modelsTickMsg:
		m.ticking = false
		if !m.hasActive || m.fetching {
			return m, nil
		}
		m.fetching = true
		return m, m.fetchDeploymentsOnly()

	case deploymentsRefreshedMsg:
		// Discard stale auto-refresh if a manual full refresh is in flight
		if m.loading {
			return m, nil
		}
		m.fetching = false
		if msg.err != nil {
			// Don't overwrite the main view — just stop refreshing
			m.hasActive = false
			return m, nil
		}
		// Update download rows in-place, remove finished, add new
		m.updateDeploymentRows(msg.pulls)
		// Check if still have active downloads
		m.hasActive = false
		for _, r := range m.rows {
			if r.sortPriority == 0 {
				m.hasActive = true
				break
			}
		}
		if m.hasActive && !m.ticking {
			m.ticking = true
			return m, m.tickCmd()
		}
		// All downloads finished — rows already transitioned to idle, no refresh needed
		return m, nil

	case routesPickerDataMsg:
		m.routeForm.availableModels = msg.models
		m.routeForm.pickerLoaded = true
		return m, nil

	case routesSavedMsg:
		if msg.err != nil {
			m.routeForm.statusMsg = msg.err.Error()
			return m, nil
		}
		m.routeForm.cancel()
		m.loading = true
		if msg.fromMode == modelsViewNewRoute && msg.group != nil {
			m.pendingRouteName = msg.group.Name
		}
		if msg.fromMode == modelsViewEditRoute || msg.fromMode == modelsViewAddDeploy || msg.fromMode == modelsViewRouteDetail {
			m.mode = modelsViewRouteDetail
		} else {
			m.mode = modelsViewList
		}
		return m, m.fetchAll()

	case routesDeletedMsg:
		m.confirmKind = confirmNone
		m.confirmMessage = ""
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Error: %v", msg.err)
			return m, nil
		}
		m.mode = modelsViewList
		m.loading = true
		return m, m.fetchAll()

	case modelPingMsg:
		m.applyPingResult(msg)
		return m, nil

	case modelsActionMsg:
		m.confirmKind, m.confirmMessage = confirmNone, ""
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Error: %v", msg.err)
			return m, nil
		}
		m.statusMsg = msg.action + " completed"
		m.applyActionResult(msg.action)
		return m, nil

	case tea.KeyPressMsg:
		// Confirmation dialog takes priority
		if m.confirmKind != confirmNone {
			return m, m.updateConfirm(msg)
		}

		switch m.mode {
		case modelsViewDetail:
			return m, m.updateDetail(msg)
		case modelsViewRouteDetail:
			return m, m.updateRouteDetail(msg)
		case modelsViewNewRoute, modelsViewEditRoute:
			return m, m.updateRouteForm(msg)
		case modelsViewAddDeploy:
			return m, m.updateDeployFormMode(msg)
		default:
			return m, m.updateList(msg)
		}
	}

	return m, nil
}

func (m *ModelsViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	if m.list.UpdateKey(msg) {
		m.statusMsg = ""
		return nil
	}

	r := m.selectedRow()

	switch {
	case key.Matches(msg, tui.ModelsKeys.Enter):
		if r == nil {
			return nil
		}
		if r.isRoute() {
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
		m.list.SetCursor(0)
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
			model := r.RawModel
			return func() tea.Msg {
				return tui.OpenParamsEditorMsg{Provider: provider, Model: model}
			}
		}
		return nil
	case key.Matches(msg, tui.ModelsKeys.Chat):
		r := m.selectedRow()
		if r == nil || r.deploy != nil {
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
		m.detail.Reset()
		return nil
	case key.Matches(msg, tui.ModelsKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *ModelsViewModel) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, tui.DefaultConfirmKeyBindings.Yes):
		// Handle route-specific confirmations
		if m.confirmKind == confirmDeleteRoute {
			// Determine route name from context (route row, child row, or detail view)
			var routeName string
			if r := m.selectedRow(); r != nil {
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
				m.confirmKind = confirmNone
				m.confirmMessage = ""
				return func() tea.Msg {
					err := client.DeleteModelGroup(routeName)
					return routesDeletedMsg{err: err}
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
	switch m.mode {
	case modelsViewDetail:
		if r := m.selectedRow(); r != nil {
			return []string{"Models", r.RawModel}
		}
		return []string{"Models", "Details"}
	case modelsViewRouteDetail:
		if g := m.selectedRouteGroup(); g != nil {
			return []string{"Models", g.Name}
		}
		return []string{"Models", "Route"}
	case modelsViewNewRoute:
		return []string{"Models", "New Route"}
	case modelsViewEditRoute:
		return []string{"Models", "Edit Route"}
	case modelsViewAddDeploy:
		return []string{"Models", "Add Deployment"}
	default:
		return []string{"Models"}
	}
}

// View implements tea.Model.
func (m *ModelsViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
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
