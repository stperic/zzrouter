package views

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// syncListItems rebuilds list items after loading, sorting or projecting rows.
func (m *ModelsViewModel) syncListItems() {
	items := make([]tui.Item, len(m.rows))
	for i := range m.rows {
		items[i] = modelRowItem{row: &m.rows[i]}
	}
	m.list.SetItems(items)
}

// buildDisplayRows merges route folders and model rows into a single display list.
// Route folders appear at the top, then standalone models.
func (m *ModelsViewModel) buildDisplayRows(modelRows []modelRow, groups []pkgClient.ModelGroupResponse) []modelRow {
	m.rawModelRows = modelRows // cache for rebuild on expand/collapse

	var rows []modelRow

	// A replica names provider, model and node. Several actual instances can
	// serve that identity (for example separate chat and embeddings endpoints).
	type modelKey struct{ model, node, provider string }
	modelIndex := make(map[modelKey][]*modelRow, len(modelRows))
	downloadIndex := make(map[modelKey][]*modelRow)
	for k := range modelRows {
		mr := &modelRows[k]
		if mr.deploy != nil {
			key := modelKey{model: mr.RawModel, node: mr.Node}
			downloadIndex[key] = append(downloadIndex[key], mr)
			continue
		}
		key := modelKey{mr.RawModel, mr.Node, mr.Provider}
		modelIndex[key] = append(modelIndex[key], mr)
	}
	lookupModels := func(d *pkgClient.ReplicaResponse) []*modelRow {
		node := d.Node
		if node == "" {
			node = m.localNode
		}
		if matches := modelIndex[modelKey{d.Model, node, d.App}]; len(matches) > 0 {
			return matches
		}
		if !m.runningOnly {
			if downloads := downloadIndex[modelKey{model: d.Model, node: node}]; len(downloads) > 0 {
				return downloads
			}
		}
		// Cloud replicas may have no node, but must still match the provider.
		if d.Node != "" {
			return modelIndex[modelKey{d.Model, "", d.App}]
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

		type child struct {
			deployment *pkgClient.ReplicaResponse
			model      *modelRow
		}
		var children []child
		statusStr := "idle"
		running := false
		if total == 0 {
			statusStr = "inactive"
		}
		for j := range g.Replicas {
			d := &g.Replicas[j]
			matches := lookupModels(d)
			if len(matches) == 0 && !m.runningOnly {
				children = append(children, child{deployment: d})
			}
			for _, mr := range matches {
				if mr.isRunning() {
					statusStr, running = "running", true
				}
				if !m.runningOnly || mr.isRunning() {
					children = append(children, child{deployment: d, model: mr})
				}
			}
		}

		if m.runningOnly && !running {
			continue
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
			for childIndex, match := range children {
				d := match.deployment

				connector := "  ├ "
				if childIndex == len(children)-1 {
					connector = "  └ "
				}

				rows = append(rows, routeChildRow(g.Name, connector, d, match.model))
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
	for _, row := range modelRows {
		if !m.runningOnly || row.isRunning() {
			rows = append(rows, row)
		}
	}

	return rows
}

// routeChildRow retains the replica's identity while displaying its current source.
func routeChildRow(routeName, connector string, replica *pkgClient.ReplicaResponse, source *modelRow) modelRow {
	row := modelRow{
		Kind: rowKindDeployChild, Node: replica.Node, Model: connector + replica.Model,
		RawModel: replica.Model, Provider: replica.App, Size: shared.EmptyValue, Status: "idle",
		sortPriority: -1, routeName: routeName, deployment: replica,
	}
	if source == nil {
		return row
	}
	row.SourceRepo, row.Node, row.Size, row.Status = source.SourceRepo, source.Node, source.Size, source.Status
	if source.deploy != nil {
		row.deploy = source.deploy
	} else {
		row.Provider, row.registry, row.instance, row.IsCloud = source.Provider, source.registry, source.instance, source.IsCloud
	}
	return row
}

// rebuildDisplayRows re-merges routes and models after expand/collapse changes.
func (m *ModelsViewModel) rebuildDisplayRows() {
	var selected rowID
	r := m.selectedRow()
	if r != nil {
		selected = r.id()
	}
	m.rows = m.buildDisplayRows(m.rawModelRows, m.groups)
	m.applySortBy()
	m.syncListItems()
	m.restoreCursor(selected, r != nil)
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

func (m *ModelsViewModel) targetsRoute() bool {
	return m.mode == modelsViewRouteDetail || m.mode == modelsViewEditRoute || m.mode == modelsViewAddDeploy
}

// selectedRouteGroup resolves the active route independently of filtered rows.
func (m *ModelsViewModel) selectedRouteGroup() *pkgClient.ModelGroupResponse {
	if m.targetsRoute() && m.activeRoute != "" {
		for i := range m.groups {
			if m.groups[i].Name == m.activeRoute {
				return &m.groups[i]
			}
		}
		return nil
	}
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
	ctx := m.beginLoad()
	client := m.client
	return func() tea.Msg {
		if err := ctx.Err(); err != nil {
			return deploymentsRefreshedMsg{ctx: ctx, err: err}
		}
		pulls, err := shared.FetchDeployments(client)
		return deploymentsRefreshedMsg{ctx: ctx, pulls: pulls, err: err}
	}
}

func (m *ModelsViewModel) beginLoad() context.Context {
	if m.loadCancel != nil {
		m.loadCancel()
	}
	m.loadContext, m.loadCancel = context.WithCancel(m.Context())
	m.fetching = true
	return m.loadContext
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
	kind     rowKind
	node     string
	model    string
	provider string
	route    string
	replica  string
	instance string
	download string
	job      string
}

func (r *modelRow) id() rowID {
	id := rowID{kind: r.Kind, node: r.Node, model: r.RawModel, provider: r.Provider, route: r.routeName}
	if r.deployment != nil {
		id.replica = r.deployment.Name
	}
	if r.instance != nil {
		id.instance = r.instance.ID
	}
	if r.deploy != nil {
		id.download = r.deploy.DownloadID
		id.job = r.deploy.JobID
	}
	return id
}

// restoreCursor moves the cursor back onto id if that row still exists, so a
// reload never retargets the detail panel at whichever row sorted into the
// old slot. have reports whether id came from a real selection.
func (m *ModelsViewModel) restoreCursor(id rowID, have bool) {
	if m.targetsRoute() {
		if m.activeRoute == "" && id.kind == rowKindRoute {
			m.activeRoute = id.model
		}
		if group := m.selectedRouteGroup(); group != nil {
			id, have = rowID{kind: rowKindRoute, model: group.Name}, true
		} else {
			m.mode, m.activeRoute = modelsViewList, ""
			m.routeForm.cancel()
			m.confirmKind, m.confirmMessage = confirmNone, ""
		}
	}
	if !have {
		return
	}
	for i := range m.rows {
		if m.rows[i].id() == id {
			m.list.SetCursor(i)
			return
		}
	}
	// Only row-owned state retires with the row. A draft or configured route
	// can remain open while its background row is filtered out.
	if m.mode == modelsViewDetail {
		m.mode = modelsViewList
	}
	m.confirmKind, m.confirmMessage = confirmNone, ""
	m.modelUsage, m.modelUsageIdle = nil, false
	m.ping = pingState{}
}

func (m *ModelsViewModel) fetchAllWithGroups(refresh bool, includeGroups bool) tea.Cmd {
	ctx := m.beginLoad()
	client := m.client
	runningOnly := m.runningOnly
	return func() tea.Msg {
		if err := ctx.Err(); err != nil {
			return modelsLoadedMsg{ctx: ctx, err: err}
		}
		rows, catalog, groups, routePrefix, warnings, err := fetchCombinedModels(ctx, client, refresh, includeGroups, runningOnly)
		var localNode string
		if err == nil && includeGroups {
			var nodeErr error
			localNode, nodeErr = localRouteNode(ctx, client, groups)
			if nodeErr != nil {
				warnings = append(warnings, "Could not identify local route node")
			}
		}
		return modelsLoadedMsg{ctx: ctx, rows: rows, catalog: catalog, groups: groups, routePrefix: routePrefix, localNode: localNode, warnings: warnings, err: err}
	}
}

// localRouteNode resolves an omitted replica node without guessing from workers.
func localRouteNode(ctx context.Context, client *pkgClient.Client, groups []pkgClient.ModelGroupResponse) (string, error) {
	needsLocal := false
	for _, group := range groups {
		for _, replica := range group.Replicas {
			if replica.Node == "" {
				needsLocal = true
			}
		}
	}
	if !needsLocal {
		return "", nil
	}
	nodes, err := client.GetNodeInventory(ctx)
	if err != nil {
		return "", err
	}
	for _, node := range nodes {
		if node.Name != "" && (node.ClusterRole == string(config.ClusterModeCoordinator) || node.ClusterRole == string(config.ClusterModeStandalone)) {
			return node.Name, nil
		}
	}
	return "", fmt.Errorf("coordinator identity is unavailable")
}

func fetchCombinedModels(ctx context.Context, client *pkgClient.Client, refresh bool, includeGroups bool, runningOnly bool) ([]modelRow, []pkgClient.ModelMetadata, []pkgClient.ModelGroupResponse, string, []string, error) {
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
	go func() { defer wg.Done(); instances, instancesErr = client.ListInstancesCtx(ctx) }()
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
		return nil, models, groups, routePrefix, warnings, modelsErr
	}
	if runningOnly && instancesErr != nil {
		return nil, models, groups, routePrefix, warnings, instancesErr
	}

	if runningOnly {
		// A simultaneous transfer must not hide the instance already serving this model.
		pulls = nil
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

	// 2. Every instance keeps its own identity. Covered only suppresses catalog rows.
	for i := range instances {
		inst := &instances[i]
		name := utils.FormatRunName(inst.Model, inst.App)
		reg := registryByModel[inst.Model]
		diskSize := int64(0)
		if reg != nil {
			diskSize = reg.Size
		}
		// The run states where its weights came from.
		sourceRepo := inst.SourceRepo
		row := modelRow{
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
		}
		if inst.RecordOnly {
			row.sortPriority = 2
		}
		rows = append(rows, row)
		if inst.Model != "" {
			covered[coveredKey(inst.Model, inst.Node)] = true
		}
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

	return rows, models, groups, routePrefix, warnings, nil
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
	status := inst.DisplayStatus()
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

// updateDeploymentRows updates download rows in-place from fresh deploy data.
// Existing download rows are updated, finished ones removed, new ones prepended.
func (m *ModelsViewModel) updateDeploymentRows(pulls []shared.DeploymentInfo) {
	if m.runningOnly {
		return
	}
	// Index current deploy rows by key for fast lookup
	pullIdx := make(map[string]int) // key → index in rawModelRows
	for i, r := range m.rawModelRows {
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
				m.rawModelRows[idx] = modelRow{
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
			m.rawModelRows[idx] = newRow
		} else {
			// New download — prepend (downloads go first)
			m.rawModelRows = append([]modelRow{newRow}, m.rawModelRows...)
			for id, idx := range pullIdx {
				pullIdx[id] = idx + 1
			}
		}
	}

	// Remove download rows that are no longer in the pulls list (cancelled externally)
	filtered := m.rawModelRows[:0]
	for _, r := range m.rawModelRows {
		if r.deploy != nil && !seen[r.deploy.DownloadID] {
			continue
		}
		filtered = append(filtered, r)
	}
	m.rawModelRows = filtered
	m.rebuildDisplayRows()
}
