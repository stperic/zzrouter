package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
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
func (r *modelRow) isRunning() bool {
	return r.instance != nil && !r.instance.RecordOnly && r.instance.Status == "running"
}

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
	ctx         context.Context
	rows        []modelRow
	catalog     []pkgClient.ModelMetadata
	groups      []pkgClient.ModelGroupResponse
	routePrefix string
	localNode   string
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
	ctx   context.Context
	pulls []shared.DeploymentInfo
	err   error
}

// modelsActionMsg is returned after an async action (start/stop/delete) completes
type modelsActionMsg struct {
	ctx             context.Context
	action          string // "start", "stop", "delete", "cancel"
	err             error
	alreadyFinished bool
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
	help tui.Help
	tui.Draft
	routeWriteContext   context.Context
	routeWriteCancel    context.CancelFunc
	routeWriteUncertain bool
	usageContext        context.Context
	usageCancel         context.CancelFunc
	usageTarget         rowID

	client      *pkgClient.Client
	styles      ui.Styles
	rows        []modelRow
	list        tui.List
	detail      tui.Detail
	termWidth   int
	termHeight  int
	loading     bool
	err         error
	hasActive   bool   // true if any downloads are in progress
	fetching    bool   // true while a fetch is in flight
	ticking     bool   // true while a pull-refresh tick timer is running
	sortBy      string // current sort column (default: "status")
	runningOnly bool
	loadContext context.Context
	loadCancel  context.CancelFunc

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
	statusMsg       string
	operationStatus string

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
	localNode      string                         // coordinator identity for replicas with an omitted node
	activeRoute    string                         // named route being viewed or edited, independent of projection

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

// NewRunningModelsViewModel opens the model view with only live running instances.
func NewRunningModelsViewModel(client *pkgClient.Client, styles ui.Styles) *ModelsViewModel {
	m := NewModelsViewModel(client, styles)
	m.runningOnly = true
	return m
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
	if m.help.Update(msg, m.mode == modelsViewList || m.mode == modelsViewDetail || m.mode == modelsViewRouteDetail) {
		return m, nil
	}

	if handled, cmd := m.Draft.Update(msg); handled {
		return m, cmd
	}
	if m.NavigationBlocked() {
		switch msg.(type) {
		case tea.KeyPressMsg, tea.MouseClickMsg, tea.MouseWheelMsg, tea.PasteMsg, tui.BreadcrumbClickMsg:
			return m, nil
		}
	}

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
		if m.NavigationBlocked() {
			return m, nil
		}
		if msg.ctx == nil || msg.ctx != m.loadContext || msg.ctx.Err() != nil {
			return m, nil
		}
		m.loading = false
		m.fetching = false
		// A failed background reload must not swap populated content for an
		// error page: keep what is on screen and report in the status line.
		if msg.err != nil && len(m.rows) > 0 {
			m.statusMsg = m.operationStatus
			if m.statusMsg != "" {
				m.statusMsg += ". "
			}
			m.statusMsg += fmt.Sprintf("Refresh failed: %v", msg.err)
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
			m.routeWriteUncertain = false
			m.groups = msg.groups
			m.localNode = msg.localNode
		}
		if msg.routePrefix != "" {
			m.routePrefix = msg.routePrefix
		}
		m.err = msg.err
		if msg.err != nil && m.operationStatus != "" {
			m.err = fmt.Errorf("%s; refresh failed: %w", m.operationStatus, msg.err)
		}
		m.statusMsg = strings.Join(append([]string{m.operationStatus}, msg.warnings...), "; ")
		m.statusMsg = strings.TrimPrefix(m.statusMsg, "; ")
		m.rows = m.buildDisplayRows(msg.rows, m.groups)
		m.applySortBy()
		m.syncListItems()
		m.restoreCursor(selected, hadSelection)
		// Keep the picker tied to catalog inventory, independent of instance projection.
		if !m.routeForm.pickerLoaded && msg.catalog != nil {
			m.routeForm.availableModels = msg.catalog
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
			for _, group := range m.groups {
				if group.Name == m.pendingRouteName {
					m.activeRoute = group.Name
					m.mode = modelsViewRouteDetail
					m.detailDepCursor = 0
					m.restoreCursor(rowID{kind: rowKindRoute, model: group.Name}, true)
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
		if msg.ctx == nil || msg.ctx != m.usageContext || msg.ctx.Err() != nil || m.mode != modelsViewDetail {
			return m, nil
		}
		if row := m.selectedRow(); row == nil || row.id() != msg.target {
			return m, nil
		}
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
		if m.loading || m.fetching || m.NavigationBlocked() {
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
		if msg.ctx == nil || msg.ctx != m.loadContext || msg.ctx.Err() != nil {
			return m, nil
		}
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

	case routeCloseMsg:
		if msg.owner != m || m.NavigationBlocked() || m.ConfirmNavigation(msg) {
			return m, nil
		}
		m.discardRouteForm()
		return m, nil
	case routesPickerDataMsg:
		m.routeForm.availableModels = msg.models
		m.routeForm.pickerLoaded = true
		return m, nil

	case routesSavedMsg:
		if !m.finishRouteWrite(msg.ctx) {
			return m, nil
		}
		if msg.err != nil {
			m.routeForm.statusMsg = msg.err.Error()
			m.routeWriteUncertain = writeOutcomeUnknown(msg.err) || msg.group != nil
			if m.routeWriteUncertain {
				m.routeForm.statusMsg = "Write outcome uncertain. Close and refresh routes before saving again. " + msg.err.Error()
			}
			m.statusMsg = m.routeForm.statusMsg
			return m, nil
		}
		m.Dirty = false
		m.routeForm.cancel()
		m.loading = true
		if msg.fromMode == modelsViewNewRoute && msg.group != nil {
			m.pendingRouteName = msg.group.Name
		}
		if msg.fromMode == modelsViewEditRoute || msg.fromMode == modelsViewAddDeploy || msg.fromMode == modelsViewRouteDetail {
			if msg.group != nil {
				m.activeRoute = msg.group.Name
			}
			m.mode = modelsViewRouteDetail
		} else {
			m.mode = modelsViewList
		}
		return m, m.fetchAll()

	case routesDeletedMsg:
		if !m.finishRouteWrite(msg.ctx) {
			return m, nil
		}
		m.confirmKind = confirmNone
		m.confirmMessage = ""
		if msg.err != nil {
			m.routeWriteUncertain = writeOutcomeUnknown(msg.err)
			m.statusMsg = fmt.Sprintf("Delete failed: %v", msg.err)
			if m.routeWriteUncertain {
				m.statusMsg = "Delete outcome unknown. Refresh routes before another write."
			}
			return m, nil
		}
		m.mode = modelsViewList
		m.loading = true
		return m, m.fetchAll()

	case modelPingMsg:
		m.applyPingResult(msg)
		return m, nil

	case modelsActionMsg:
		if msg.ctx == nil || msg.ctx != m.Context() || msg.ctx.Err() != nil {
			return m, nil
		}
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Error: %v", msg.err)
			m.operationStatus = m.statusMsg
			return m, nil
		}
		m.statusMsg = msg.action + " completed"
		if msg.alreadyFinished {
			m.statusMsg = "Download already finished"
		}
		m.operationStatus = m.statusMsg
		return m, m.Refresh()

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
