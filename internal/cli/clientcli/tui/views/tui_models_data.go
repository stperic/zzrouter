package views

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// buildDisplayRows merges route folders and model rows into a single display list.
// Route folders appear at the top, then standalone models.
func (m *ModelsViewModel) buildDisplayRows(modelRows []modelRow, groups []pkgClient.ModelGroupResponse) []modelRow {
	m.rawModelRows = modelRows // cache for rebuild on expand/collapse

	var rows []modelRow

	// Build lookup index: (model, node) → modelRow for O(1) child row matching.
	// Key is "model\x00node"; entries with empty node also indexed as "model\x00".
	type modelKey struct{ model, node string }
	modelIndex := make(map[modelKey]*modelRow, len(modelRows))
	for k := range modelRows {
		mr := &modelRows[k]
		modelIndex[modelKey{mr.RawModel, mr.Node}] = mr
	}

	// lookupModel finds the best matching modelRow for a deployment.
	lookupModel := func(model, node string) *modelRow {
		if mr := modelIndex[modelKey{model, node}]; mr != nil {
			return mr
		}
		// Fallback: node-agnostic match (for cloud deployments with empty node)
		if node != "" {
			return modelIndex[modelKey{model, ""}]
		}
		return nil
	}

	// 1. Route header rows (with child rows if expanded)
	for i := range groups {
		g := &groups[i]
		total := len(g.Replicas)

		icon := "▷ "
		if m.expandedRoutes[g.Name] {
			icon = "▶ "
		}

		// Aggregate status from matched model rows
		statusStr := "idle"
		if total == 0 {
			statusStr = "inactive"
		} else {
			for _, d := range g.Replicas {
				if mr := lookupModel(d.Model, d.Node); mr != nil && strings.HasPrefix(mr.Status, "running") {
					statusStr = "running"
					break
				}
			}
		}

		rows = append(rows, modelRow{
			Kind:         rowKindRoute,
			Model:        icon + g.Name,
			RawModel:     g.Name,
			Status:       statusStr,
			sortPriority: -1,
			route:        g,
		})

		// If expanded, add child deployment rows showing same info as standalone models
		if m.expandedRoutes[g.Name] {
			for j := range g.Replicas {
				d := &g.Replicas[j]

				connector := "  ├ "
				if j == len(g.Replicas)-1 {
					connector = "  └ "
				}

				childRow := modelRow{
					Kind:         rowKindDeployChild,
					Node:         d.Node,
					Model:        connector + d.Model,
					RawModel:     d.Model,
					Provider:     d.App,
					Size:         shared.EmptyValue,
					Status:       "idle",
					sortPriority: -1,
					routeName:    g.Name,
					deployment:   d,
				}

				if mr := lookupModel(d.Model, d.Node); mr != nil {
					childRow.Model = connector + d.Model
					childRow.Provider = mr.Provider
					childRow.SourceRepo = mr.SourceRepo
					childRow.Node = mr.Node
					childRow.Size = mr.Size
					childRow.Status = mr.Status
					childRow.registry = mr.registry
					childRow.instance = mr.instance
					childRow.IsCloud = mr.IsCloud
				}

				rows = append(rows, childRow)
			}
		}
	}

	// Prune stale expandedRoutes entries
	validRoutes := make(map[string]bool, len(groups))
	for _, g := range groups {
		validRoutes[g.Name] = true
	}
	for name := range m.expandedRoutes {
		if !validRoutes[name] {
			delete(m.expandedRoutes, name)
		}
	}

	// 2. Standalone model rows (downloads, running, idle)
	rows = append(rows, modelRows...)

	return rows
}

// rebuildDisplayRows re-merges routes and models after expand/collapse changes.
func (m *ModelsViewModel) rebuildDisplayRows() {
	m.rows = m.buildDisplayRows(m.rawModelRows, m.groups)
	m.applySortBy()
	m.syncListItems()
}

// applySortBy sorts m.rows according to the current sortBy field.
// Routes (sortPriority == -1) always stay at the top, followed by their children.
func (m *ModelsViewModel) applySortBy() {
	sortBy := m.sortBy
	if sortBy == "" {
		sortBy = "status"
	}
	sort.SliceStable(m.rows, func(i, j int) bool {
		ri, rj := &m.rows[i], &m.rows[j]
		// Routes and their children stay grouped at the top
		if ri.isRoute() || ri.isChild() || rj.isRoute() || rj.isChild() {
			return false
		}
		switch sortBy {
		case "model":
			return strings.ToLower(ri.RawModel) < strings.ToLower(rj.RawModel)
		case "app":
			return strings.ToLower(ri.Provider) < strings.ToLower(rj.Provider)
		case "node":
			return strings.ToLower(ri.Node) < strings.ToLower(rj.Node)
		default: // "status" — running first, downloading next, idle last
			return ri.sortPriority < rj.sortPriority
		}
	})
}

// selectedRouteGroup returns the ModelGroupResponse for the currently selected route row.
func (m *ModelsViewModel) selectedRouteGroup() *pkgClient.ModelGroupResponse {
	r := m.selectedRow()
	if r == nil || !r.isRoute() {
		return nil
	}
	// Find the fresh group from m.groups
	for i := range m.groups {
		if m.groups[i].Name == r.RawModel {
			return &m.groups[i]
		}
	}
	return r.route
}

func (m *ModelsViewModel) fetchDeploymentsOnly() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		pulls, err := shared.FetchDeployments(client)
		return deploymentsRefreshedMsg{pulls: pulls, err: err}
	}
}

func (m *ModelsViewModel) fetchAll() tea.Cmd {
	return m.fetchAllWithGroups(true, true)
}

// Refresh implements tui.Refresher. A child view (chat, params editor) may
// have started or stopped the selected model, so reload on the way back.
//
// refresh=false skips the registry rescan, which fans out to every worker
// node and is what made returning from chat visibly stall. Only instance and
// deployment state can have changed, and both are coordinator-local. Groups
// stay in: starting a model can mint an auto-route, and reading it is one
// more local call.
//
// The guards are reset rather than respected, and both poll chains re-armed.
// While the child was on top it was the only view receiving messages, so any
// reply this view was waiting on — and every timer it had pending — went
// there and was dropped. The flags say "in flight" but nothing is.
func (m *ModelsViewModel) Refresh() tea.Cmd {
	m.loading = false
	m.fetching = true
	m.ticking = false
	m.pollEpoch++

	cmds := []tea.Cmd{m.fetchAllWithGroups(false, true), m.autoRefreshCmd()}
	if !m.spinning {
		m.spinning = true
		cmds = append(cmds, shared.SpinnerTickCmd())
	}
	return tea.Batch(cmds...)
}

// rowID identifies a row independently of its position, so the cursor can
// follow the same entry when a status change re-sorts the list under it.
// route is part of the key because two groups can list the same replica,
// giving their child rows an otherwise identical kind/node/model.
type rowID struct {
	kind  rowKind
	node  string
	model string
	route string
}

func (r *modelRow) id() rowID {
	return rowID{kind: r.Kind, node: r.Node, model: r.RawModel, route: r.routeName}
}

// restoreCursor moves the cursor back onto id if that row still exists, so a
// reload never retargets the detail panel at whichever row sorted into the
// old slot. have reports whether id came from a real selection.
func (m *ModelsViewModel) restoreCursor(id rowID, have bool) {
	if !have {
		return
	}
	for i := range m.rows {
		if m.rows[i].id() == id {
			m.list.SetCursor(i)
			return
		}
	}
}

func (m *ModelsViewModel) fetchAllWithGroups(refresh bool, includeGroups bool) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		rows, groups, routePrefix, warnings, err := fetchCombinedModels(client, refresh, includeGroups)
		return modelsLoadedMsg{rows: rows, groups: groups, routePrefix: routePrefix, warnings: warnings, err: err}
	}
}

func fetchCombinedModels(client *pkgClient.Client, refresh bool, includeGroups bool) ([]modelRow, []pkgClient.ModelGroupResponse, string, []string, error) {
	var (
		models       []pkgClient.ModelMetadata
		modelsErr    error
		instances    []pkgClient.Instance
		instancesErr error
		pulls        []shared.DeploymentInfo
		pullsErr     error
		groups       []pkgClient.ModelGroupResponse
		groupsErr    error
		routePrefix  string
	)

	// Fire independent API calls concurrently.
	// Groups are fetched AFTER models because the models request triggers server-side
	// cache refresh which creates auto-routes. Fetching groups concurrently would race
	// with auto-route creation and return stale data on cold start.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		models, modelsErr = shared.FetchModels(client, &shared.ModelQueryParams{Node: "*", Provider: "*", SortBy: "date"}, refresh)
	}()
	go func() { defer wg.Done(); instances, instancesErr = shared.FetchInstances(client, "", "") }()
	go func() { defer wg.Done(); pulls, pullsErr = shared.FetchDeployments(client) }()
	wg.Wait()

	if includeGroups {
		groups, routePrefix, groupsErr = client.ListModelGroups()
	}

	var warnings []string
	if instancesErr != nil {
		warnings = append(warnings, "Could not load instances")
	}
	if pullsErr != nil {
		warnings = append(warnings, "Could not load downloads")
	}
	if groupsErr != nil {
		warnings = append(warnings, "Could not load routes")
	}

	if modelsErr != nil {
		return nil, groups, routePrefix, warnings, modelsErr
	}

	runningByModel := make(map[string]*pkgClient.Instance, len(instances))
	for i := range instances {
		runningByModel[instances[i].Model] = &instances[i]
	}

	downloadByModel := make(map[string]*shared.DeploymentInfo, len(pulls))
	for i := range pulls {
		downloadByModel[pulls[i].Model] = &pulls[i]
	}

	covered := make(map[string]bool)
	var rows []modelRow

	// coveredKey uniquely identifies a model on a specific node
	coveredKey := func(model, node string) string { return model + "\x00" + node }

	// 1. Downloads first (priority 0)
	// Show active and failed downloads. Completed downloads are skipped
	// because they appear as registry models in section 3.
	for i := range pulls {
		p := &pulls[i]
		if p.Status == constants.StatusCompleted {
			continue
		}
		// p.Provider is the *source registry* (huggingface/ollama), not an
		// execution provider — keep it only in SourceRepo and show "-" in the
		// PROVIDER column until AutoAssignProvider picks a real one.
		rows = append(rows, modelRow{
			Node:         p.Node,
			Model:        shared.FormatModelWithRegistry(p.Provider, utils.FormatModelName(p.Model)),
			RawModel:     p.Model,
			Provider:     shared.EmptyValue,
			SourceRepo:   p.Provider,
			Size:         p.Size,
			Status:       formatDeploymentStatus(p),
			sortPriority: 0,
			deploy:       p,
		})
		covered[coveredKey(p.Model, p.Node)] = true
	}

	// Build a lookup of registry models by name (any node)
	// so running instances can show metadata and disk sizes
	registryByModel := make(map[string]*pkgClient.ModelMetadata, len(models))
	for i := range models {
		registryByModel[models[i].Name] = &models[i]
	}

	// 2. Running instances (priority 1)
	for i := range instances {
		inst := &instances[i]
		if covered[coveredKey(inst.Model, inst.Node)] {
			continue
		}
		name := utils.FormatModelName(inst.Model)
		if name == "" {
			name = "(unknown)"
		}
		reg := registryByModel[inst.Model]
		diskSize := int64(0)
		if reg != nil {
			diskSize = reg.Size
		}
		// The run states where its weights came from.
		sourceRepo := inst.SourceRepo
		rows = append(rows, modelRow{
			Node:         inst.Node,
			Model:        shared.FormatModelWithRegistry(sourceRepo, name),
			RawModel:     inst.Model,
			Provider:     inst.App,
			SourceRepo:   sourceRepo,
			Size:         formatRunningSize(inst, diskSize),
			Status:       formatInstanceStatus(inst),
			sortPriority: 1,
			instance:     inst,
			registry:     reg, // attach registry for metadata in detail view
		})
		covered[coveredKey(inst.Model, inst.Node)] = true
	}

	// 3. Registry models not running or downloading (priority 2)
	for i := range models {
		m := &models[i]
		if covered[coveredKey(m.Name, m.Node)] {
			continue
		}
		repo := m.SourceRepo
		if repo == "" {
			repo = constants.RepoOllama
		}
		provider := m.AssignedApp
		if provider == "" {
			provider = shared.EmptyValue
		}
		node := m.Node
		if m.IsCloud {
			node = constants.CloudNodeName
		}
		rows = append(rows, modelRow{
			Node:         node,
			Model:        shared.FormatModelWithRegistry(repo, utils.FormatModelName(m.Name)),
			RawModel:     m.Name,
			Provider:     provider,
			SourceRepo:   repo,
			Size:         shared.FormatSize(m.Size),
			Status:       "idle",
			IsCloud:      m.IsCloud,
			sortPriority: 2,
			registry:     m,
		})
	}

	return rows, groups, routePrefix, warnings, nil
}

func formatDeploymentStatus(p *shared.DeploymentInfo) string {
	switch p.Status {
	case constants.StatusDownloading:
		if p.Progress != shared.EmptyValue {
			s := "⬇ " + p.Progress
			if p.Speed != shared.EmptyValue {
				s += " " + p.Speed
			}
			if p.ETA != "" && p.ETA != shared.EmptyValue {
				s += " (" + p.ETA + ")"
			}
			return s
		}
		return "⬇ starting"
	case constants.StatusPending:
		return "⬇ starting"
	case constants.StatusFailed:
		return "failed"
	case constants.StatusSkipped:
		return "skipped"
	default:
		return string(p.Status)
	}
}

func formatInstanceStatus(inst *pkgClient.Instance) string {
	until := formatCompactUntil(inst.KeepAlive, inst.LastActivity, inst.StartedAt)
	status := inst.Status
	if until == "forever" {
		return status
	}
	if until != "" {
		return status + " (" + until + " left)"
	}
	return status
}

// formatCompactUntil returns a compact expiry string like "20m", "3h", "Forever"
// suitable for TUI column display.
func formatCompactUntil(keepAlive, lastActivity, startedAt string) string {
	if keepAlive == "" {
		return ""
	}

	activityTime := lastActivity
	if activityTime == "" {
		activityTime = startedAt
	}
	if activityTime == "" {
		return ""
	}

	duration, err := time.ParseDuration(keepAlive)
	if err != nil {
		return ""
	}

	if duration < 0 || duration > 100*365*24*time.Hour {
		return "forever"
	}

	parsedActivityTime, err := time.Parse(time.RFC3339, activityTime)
	if err != nil {
		return ""
	}

	remaining := time.Until(parsedActivityTime.Add(duration))
	if remaining <= time.Second {
		return ""
	}

	switch {
	case remaining < time.Minute:
		return fmt.Sprintf("%ds", int(math.Ceil(remaining.Seconds())))
	case remaining < time.Hour:
		return fmt.Sprintf("%dm", int(math.Ceil(remaining.Minutes())))
	case remaining < 24*time.Hour:
		h := int(remaining.Hours())
		m := int(math.Ceil(float64(int(remaining.Seconds())%3600) / 60))
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dd", int(math.Ceil(remaining.Hours()/24)))
	}
}

// formatRunningSize shows "disk / mem" for running instances when both are known.
func formatRunningSize(inst *pkgClient.Instance, diskSize int64) string {
	memSize := inst.SizeBytes
	if diskSize > 0 && memSize > 0 && diskSize != memSize {
		return shared.FormatSize(diskSize) + " / " + shared.FormatSize(memSize)
	}
	if memSize > 0 {
		return shared.FormatSize(memSize)
	}
	if diskSize > 0 {
		return shared.FormatSize(diskSize)
	}
	return shared.EmptyValue
}

// selectedRow returns the currently selected row, or nil if none.
func (m *ModelsViewModel) selectedRow() *modelRow {
	c := m.list.Cursor()
	if c >= 0 && c < len(m.rows) {
		return &m.rows[c]
	}
	return nil
}

// applyActionResult updates rows locally after a successful action.
func (m *ModelsViewModel) applyActionResult(action string) {
	r := m.selectedRow()
	if r == nil {
		return
	}
	switch action {
	case "stop":
		r.Status = "idle"
		r.instance = nil
		if r.registry != nil {
			r.Size = shared.FormatSize(r.registry.Size)
		} else {
			r.Size = shared.EmptyValue
		}
	case "delete":
		c := m.list.Cursor()
		m.rows = append(m.rows[:c], m.rows[c+1:]...)
		m.syncListItems()
		if c >= len(m.rows) && len(m.rows) > 0 {
			m.list.SetCursor(len(m.rows) - 1)
		}
	}
}

// updateDeploymentRows updates download rows in-place from fresh deploy data.
// Existing download rows are updated, finished ones removed, new ones prepended.
func (m *ModelsViewModel) updateDeploymentRows(pulls []shared.DeploymentInfo) {
	// Index current deploy rows by key for fast lookup
	pullIdx := make(map[string]int) // key → index in m.rows
	for i, r := range m.rows {
		if r.deploy != nil {
			pullIdx[r.deploy.DownloadID] = i
		}
	}

	seen := make(map[string]bool, len(pulls))

	for i := range pulls {
		p := &pulls[i]
		seen[p.DownloadID] = true

		if p.Status == constants.StatusCompleted {
			// Completed: transition to idle in-place. Provider stays "-" until
			// the next full refresh replaces this with a real registry row.
			if idx, exists := pullIdx[p.DownloadID]; exists {
				m.rows[idx] = modelRow{
					Node:         p.Node,
					Model:        shared.FormatModelWithRegistry(p.Provider, utils.FormatModelName(p.Model)),
					RawModel:     p.Model,
					Provider:     shared.EmptyValue,
					SourceRepo:   p.Provider,
					Size:         p.Size,
					Status:       "idle",
					sortPriority: 2,
					deploy:       nil, // no longer a download
				}
			}
			continue
		}

		newRow := modelRow{
			Node:         p.Node,
			Model:        shared.FormatModelWithRegistry(p.Provider, utils.FormatModelName(p.Model)),
			RawModel:     p.Model,
			Provider:     shared.EmptyValue,
			SourceRepo:   p.Provider,
			Size:         p.Size,
			Status:       formatDeploymentStatus(p),
			sortPriority: 0,
			deploy:       p,
		}

		if idx, exists := pullIdx[p.DownloadID]; exists {
			// Update existing row in-place
			m.rows[idx] = newRow
		} else {
			// New download — prepend (downloads go first)
			m.rows = append([]modelRow{newRow}, m.rows...)
			c := m.list.Cursor()
			if c > 0 {
				m.list.SetCursor(c + 1)
			}
		}
	}

	// Remove download rows that are no longer in the pulls list (cancelled externally)
	filtered := m.rows[:0]
	for _, r := range m.rows {
		if r.deploy != nil && !seen[r.deploy.DownloadID] {
			continue
		}
		filtered = append(filtered, r)
	}
	m.rows = filtered

	m.syncListItems()
}
