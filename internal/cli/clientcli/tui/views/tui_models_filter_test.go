package views

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func instanceRow(model, id, status string) modelRow {
	return modelRow{
		RawModel: model, Model: model, Node: "worker", Provider: "engine", Status: status, sortPriority: 1,
		instance: &pkgClient.Instance{ID: id, Node: "worker", App: "engine", Model: model, Status: status},
	}
}

func loadModelRows(m *ModelsViewModel, rows []modelRow, groups []pkgClient.ModelGroupResponse) {
	m.Update(modelsLoadedMsg{ctx: m.beginLoad(), rows: rows, groups: groups})
}

func TestModelsRunningProjectionUsesInstanceState(t *testing.T) {
	styles := ui.NewStyles(ui.CatppuccinMocha())
	m := NewRunningModelsViewModel(nil, styles)
	t.Cleanup(m.Cancel)
	live := instanceRow("live", "live-run", "running")
	live.Status = "display text is not authority"
	pin := instanceRow("pin", "pin-request", "running")
	pin.instance.RecordOnly = true
	stopped := instanceRow("stopped", "stopped-run", "stopped")
	stopped.Status = "running with misleading display text"
	rows := []modelRow{
		live, pin, stopped, instanceRow("starting", "starting-run", "starting"), instanceRow("stopping", "stopping-run", "stopping"),
		instanceRow("failed", "failed-run", "failed"), instanceRow("", "node-run", "running"),
		{RawModel: "download", Status: "running", deploy: &shared.DeploymentInfo{}, sortPriority: 0},
		{RawModel: "cloud-ready", IsCloud: true, Status: "running", sortPriority: 2},
	}
	groups := []pkgClient.ModelGroupResponse{
		{Name: "mixed", Replicas: []pkgClient.ReplicaResponse{{Name: "live", Model: "live", Node: "worker", App: "engine"}, {Name: "cold", Model: "stopped", Node: "worker", App: "engine"}}},
		{Name: "cold", Replicas: []pkgClient.ReplicaResponse{{Model: "stopped", Node: "worker", App: "engine"}}},
	}
	m.expandedRoutes["mixed"] = true
	loadModelRows(m, rows, groups)
	require.Len(t, m.rows, 4, "running route, its live child, live model, and node-scoped instance")
	require.True(t, m.rows[0].isRoute())
	require.Equal(t, "running", m.rows[0].Status)
	require.True(t, m.rows[1].isChild())
	require.Contains(t, m.rows[1].Model, "└", "the last visible child has the last-child connector")
	require.Len(t, m.rows[0].route.Replicas, 2, "route actions retain the complete definition")
	for _, row := range m.rows[1:] {
		require.True(t, row.isRunning())
	}
	m.rebuildDisplayRows()
	require.Len(t, m.rows, 4)
	m.updateDeploymentRows([]shared.DeploymentInfo{{Model: "new-download", Status: constants.StatusDownloading}})
	require.Len(t, m.rows, 4, "download updates cannot insert a non-running row")
	available := NewModelsViewModel(nil, styles)
	t.Cleanup(available.Cancel)
	loadModelRows(available, rows, groups)
	require.Len(t, available.rows, len(rows)+len(groups), "Available retains every model state and route")
}

func TestModelsRefreshPreservesCurrentSelectionAndClearsRemovedConfirmation(t *testing.T) {
	m := NewRunningModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	a, b := instanceRow("A", "a", "running"), instanceRow("B", "b", "running")
	loadModelRows(m, []modelRow{a, b}, nil)
	old := m.beginLoad()
	current := m.beginLoad()
	m.list.SetCursor(1)
	m.mode = modelsViewDetail
	m.confirmKind, m.confirmMessage = confirmStopModel, "Stop B?"
	m.Update(modelsLoadedMsg{ctx: current, rows: []modelRow{b, a}})
	require.Equal(t, b.id(), m.selectedRow().id(), "refresh restores the selection current when its reply arrives")
	require.Equal(t, confirmStopModel, m.confirmKind)
	m.Update(modelsLoadedMsg{ctx: old, rows: []modelRow{a}, err: errors.New("obsolete")})
	require.Equal(t, b.id(), m.selectedRow().id())
	require.Equal(t, confirmStopModel, m.confirmKind, "a retired refresh cannot clear a newer confirmation")
	loadModelRows(m, []modelRow{a}, nil)
	require.Equal(t, modelsViewList, m.mode)
	require.Equal(t, confirmNone, m.confirmKind)
	require.Empty(t, m.confirmMessage)
	m.mode = modelsViewDetail
	m.confirmKind = confirmStopModel
	loadModelRows(m, []modelRow{instanceRow("A", "replacement-a", "running")}, nil)
	require.Equal(t, modelsViewList, m.mode, "a replacement process is a different action target")
	require.Equal(t, confirmNone, m.confirmKind)
}

func TestModelsQueuedRepliesCannotCrossViews(t *testing.T) {
	styles := ui.NewStyles(ui.CatppuccinMocha())
	first := NewModelsViewModel(nil, styles)
	t.Cleanup(first.Cancel)
	second := NewRunningModelsViewModel(nil, styles)
	t.Cleanup(second.Cancel)
	ctx := first.beginLoad()
	loadModelRows(second, []modelRow{instanceRow("current", "current", "running")}, nil)
	second.Update(modelsLoadedMsg{ctx: ctx, rows: []modelRow{instanceRow("other", "other", "running")}})
	require.Equal(t, "current", second.rows[0].RawModel)
	second.Update(modelsActionMsg{ctx: first.Context(), action: "delete"})
	require.Empty(t, second.statusMsg)
	second.Cancel()
	second.Update(modelsLoadedMsg{ctx: second.loadContext, err: errors.New("late after pop")})
	require.Empty(t, second.statusMsg)
}

func TestModelsUnnamedRunsAndSortingKeepSelection(t *testing.T) {
	m := NewRunningModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	a, b := instanceRow("", "node-a", "running"), instanceRow("", "node-b", "running")
	a.Provider, b.Provider = "zeta", "alpha"
	loadModelRows(m, []modelRow{a, b}, nil)
	m.list.SetCursor(1)
	loadModelRows(m, []modelRow{b, a}, nil)
	require.Equal(t, "node-b", m.selectedRow().instance.ID)
	m.sortBy = "model"
	m.updateList(tea.KeyPressMsg{Code: 's'})
	require.Equal(t, "node-b", m.selectedRow().instance.ID)
	m.Update(tui.BreadcrumbClickMsg{Level: 1})
	require.Equal(t, []string{"Models", "Running"}, m.Breadcrumb())
}

func TestModelsDownloadRefreshSurvivesRouteRebuild(t *testing.T) {
	m := NewModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	live := instanceRow("live", "run", "running")
	loadModelRows(m, []modelRow{
		{RawModel: "old-download", deploy: &shared.DeploymentInfo{DownloadID: "old", Model: "old-download"}}, live,
	}, []pkgClient.ModelGroupResponse{{Name: "route", Replicas: []pkgClient.ReplicaResponse{{Model: "live", Node: "worker", App: "engine"}}}})
	m.restoreCursor(live.id(), true)
	m.updateDeploymentRows([]shared.DeploymentInfo{
		{DownloadID: "new", Model: "new-download", Status: constants.StatusDownloading, Progress: "5%"},
		{DownloadID: "old", Model: "old-download", Status: constants.StatusDownloading, Progress: "50%"},
	})
	m.expandedRoutes["route"] = true
	m.rebuildDisplayRows()
	require.Equal(t, live.id(), m.selectedRow().id())
	progress := map[string]string{}
	for _, row := range m.rows {
		if row.deploy != nil {
			progress[row.deploy.DownloadID] = row.deploy.Progress
		}
	}
	require.Equal(t, map[string]string{"new": "5%", "old": "50%"}, progress)
}

func TestRunningFetchKeepsInstanceDuringSameModelDownload(t *testing.T) {
	var failRuns atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		switch r.URL.Path {
		case "/zzrouter/v1/runs":
			if failRuns.Load() {
				http.Error(w, "synthetic run lookup failure", http.StatusServiceUnavailable)
				return
			}
			data = []pkgClient.Instance{{ID: "live", Node: "worker", App: "engine", Model: "same", Status: "running"}}
		case "/zzrouter/v1/deployments":
			data = []pkgClient.Deployment{{ID: "download", Model: "same", Registry: "huggingface", Nodes: []pkgClient.DeploymentNode{{Node: "worker", DownloadID: "download", Status: constants.StatusDownloading}}}}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer server.Close()
	c := pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL})
	rows, _, _, _, _, err := fetchCombinedModels(t.Context(), c, false, false, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].instance)
	require.Equal(t, "live", rows[0].instance.ID)
	failRuns.Store(true)
	_, _, _, _, _, err = fetchCombinedModels(t.Context(), c, false, false, true)
	require.Error(t, err, "unavailable run state must not be presented as no running instances")
}

func TestModelsRunningRoutesMatchEachReplicaInstance(t *testing.T) {
	m := NewRunningModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	chat := instanceRow("same", "chat", "running")
	embeddings := instanceRow("same", "embeddings", "running")
	starting := instanceRow("same", "starting", "starting")
	groups := []pkgClient.ModelGroupResponse{
		{Name: "matching", Replicas: []pkgClient.ReplicaResponse{{Name: "worker-engine", Model: "same", Node: "worker", App: "engine"}}},
		{Name: "other-provider", Replicas: []pkgClient.ReplicaResponse{{Name: "worker-other", Model: "same", Node: "worker", App: "other"}}},
	}
	m.expandedRoutes["matching"], m.expandedRoutes["other-provider"] = true, true
	loadModelRows(m, []modelRow{chat, embeddings, starting}, groups)
	require.Len(t, m.rows, 5, "one route, two distinct running children, two standalone instances")
	require.Equal(t, "matching", m.rows[0].RawModel)
	children := map[string]bool{}
	for _, row := range m.rows {
		if row.isChild() {
			require.True(t, row.isRunning())
			require.NotNil(t, row.deployment)
			assert.Equal(t, "worker-engine", row.deployment.Name, "route edit actions retain their replica identity")
			children[row.instance.ID] = true
		}
	}
	assert.Equal(t, map[string]bool{"chat": true, "embeddings": true}, children)
}

func TestRunningFetchKeepsPerEndpointInstances(t *testing.T) {
	for _, firstStatus := range []string{"starting", "running"} {
		t.Run(firstStatus, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any = []any{}
				if r.URL.Path == "/zzrouter/v1/runs" {
					data = []map[string]any{
						{"id": "embeddings-run", "node": "worker", "provider": "engine", "model": "same", "status": firstStatus, "endpoint": "embeddings"},
						{"id": "chat-run", "node": "worker", "provider": "engine", "model": "same", "status": "running", "endpoint": "chat"},
					}
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			defer server.Close()
			client := pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL})
			for _, runningOnly := range []bool{false, true} {
				rows, _, _, _, _, err := fetchCombinedModels(t.Context(), client, false, false, runningOnly)
				require.NoError(t, err)
				m := NewModelsViewModel(client, ui.NewStyles(ui.CatppuccinMocha()))
				t.Cleanup(m.Cancel)
				m.runningOnly = runningOnly
				loadModelRows(m, rows, nil)
				want := 2
				if runningOnly && firstStatus != "running" {
					want = 1
				}
				assert.Len(t, m.rows, want, "instances have independent identities even for the same model/node/provider")
			}
		})
	}
}

func TestModelsReplicaNameKeepsPendingRemovalTarget(t *testing.T) {
	for _, runningOnly := range []bool{false, true} {
		m := NewModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
		t.Cleanup(m.Cancel)
		m.runningOnly = runningOnly
		live := instanceRow("same", "live", "running")
		group := pkgClient.ModelGroupResponse{Name: "shared", Replicas: []pkgClient.ReplicaResponse{
			{Name: "first", Model: "same", Node: "worker", App: "engine"},
			{Name: "second", Model: "same", Node: "worker", App: "engine"},
		}}
		m.expandedRoutes[group.Name] = true
		loadModelRows(m, []modelRow{live}, []pkgClient.ModelGroupResponse{group})
		m.list.SetCursor(2)
		require.Equal(t, "second", m.selectedRow().deployment.Name)
		m.confirmKind, m.confirmMessage = confirmDeleteDeployList, "Remove second?"
		m.rebuildDisplayRows()
		assert.Equal(t, "second", m.selectedRow().deployment.Name)
		assert.Equal(t, confirmDeleteDeployList, m.confirmKind)
	}
}

func TestModelsRoutePickerUsesCompleteCatalogOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		switch r.URL.Path {
		case "/zzrouter/v1/runs":
			data = []pkgClient.Instance{
				{ID: "chat", Model: "same", Node: "worker", App: "engine", Status: "running"},
				{ID: "embeddings", Model: "same", Node: "worker", App: "engine", Status: "running"},
			}
		case "/zzrouter/v1/models":
			data = []pkgClient.ModelMetadata{
				{Name: "same", Node: "worker", AssignedApp: "engine"},
				{Name: "transferring", Node: "worker", AssignedApp: "engine"},
			}
		case "/zzrouter/v1/deployments":
			data = []pkgClient.Deployment{{ID: "download", Model: "transferring", Nodes: []pkgClient.DeploymentNode{{Node: "worker", Status: constants.StatusDownloading}}}}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer server.Close()
	for _, runningOnly := range []bool{false, true} {
		m := NewModelsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
		t.Cleanup(m.Cancel)
		m.runningOnly = runningOnly
		m.Update(m.fetchAll()())
		var names []string
		for _, model := range m.routeForm.availableModels {
			names = append(names, model.Name)
		}
		assert.ElementsMatch(t, []string{"same", "transferring"}, names)
	}
}

func TestRunningLocalRouteUsesCoordinatorIdentity(t *testing.T) {
	for _, role := range []string{"coordinator", "standalone", "worker"} {
		t.Run(role, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any = []any{}
				switch r.URL.Path {
				case "/zzrouter/v1/runs":
					data = []pkgClient.Instance{
						{ID: "remote", Node: "remote", App: "engine", Model: "same", Status: "running"},
						{ID: "local", Node: "office", App: "engine", Model: "same", Status: "running"},
					}
				case "/zzrouter/v1/model-groups":
					data = []pkgClient.ModelGroupResponse{{Name: "local-route", Replicas: []pkgClient.ReplicaResponse{{Name: "local", App: "engine", Model: "same"}}}}
				case "/zzrouter/v1/nodes":
					data = []map[string]any{{"name": "remote", "cluster_role": "worker"}, {"name": "office", "cluster_role": role}}
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			defer server.Close()
			m := NewRunningModelsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			m.expandedRoutes["local-route"] = true
			m.Update(m.fetchAll()())
			require.Len(t, m.groups, 1)
			assert.Empty(t, m.groups[0].Replicas[0].Node, "route edits retain the original local-node definition")
			if role == "worker" {
				assert.Len(t, m.rows, 2, "unknown local identity must not borrow the first worker")
				assert.Contains(t, m.statusMsg, "local route node")
				return
			}
			require.Len(t, m.rows, 4)
			require.True(t, m.rows[1].isChild())
			assert.Equal(t, "local", m.rows[1].instance.ID)
		})
	}
}

func TestModelsCancelInterruptsLocalNodeRead(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zzrouter/v1/nodes" {
			close(started)
			<-r.Context().Done()
			return
		}
		var data any = []any{}
		if r.URL.Path == "/zzrouter/v1/model-groups" {
			data = []pkgClient.ModelGroupResponse{{Name: "local-route", Replicas: []pkgClient.ReplicaResponse{{Name: "local", App: "engine", Model: "same"}}}}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)
	m := NewRunningModelsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	cmd := m.fetchAll()
	replies := make(chan tea.Msg, 1)
	go func() { replies <- cmd() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("local node read did not start")
	}
	m.Cancel()
	select {
	case reply := <-replies:
		m.Update(reply)
		assert.Empty(t, m.groups, "canceled reads cannot populate a retired view")
	case <-time.After(time.Second):
		t.Fatal("local node read did not honor view cancellation")
	}
}

func TestModelsOperationRefreshRetainsFeedbackAndUsesServerState(t *testing.T) {
	for _, action := range []string{"stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			var failReads, stopped atomic.Bool
			requests := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					requests <- r.URL.String()
					if r.URL.Path == "/zzrouter/v1/runs/a" {
						stopped.Store(true)
					}
					if r.URL.Path == "/zzrouter/v1/models" {
						var body struct {
							Models []pkgClient.ModelMetadata `json:"models"`
						}
						if assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) && assert.Len(t, body.Models, 1) {
							assert.Equal(t, "A", body.Models[0].Name)
							assert.Equal(t, "worker", body.Models[0].Node)
						}
					}
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"deleted": 1}}))
					return
				}
				if failReads.Load() {
					http.Error(w, "synthetic refresh failure", http.StatusServiceUnavailable)
					return
				}
				var data any = []any{}
				if r.URL.Path == "/zzrouter/v1/runs" {
					instances := []pkgClient.Instance{{ID: "b", Model: "B", Node: "worker", App: "engine", Status: "running"}}
					if !stopped.Load() {
						instances = append(instances, pkgClient.Instance{ID: "a", Model: "A", Node: "worker", App: "engine", Status: "running"})
					}
					data = instances
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			defer server.Close()
			m := NewRunningModelsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			a, b := instanceRow("A", "a", "running"), instanceRow("B", "b", "running")
			a.registry = &pkgClient.ModelMetadata{Name: "A", Node: "worker", AssignedApp: "engine"}
			loadModelRows(m, []modelRow{a, b}, nil)
			m.mode = modelsViewDetail
			m.confirmKind = confirmStopModel
			if action == "delete" {
				m.confirmKind = confirmDeleteModel
			}
			failReads.Store(true)
			operation := m.executeConfirmedAction()
			require.NotNil(t, operation)
			require.Equal(t, confirmNone, m.confirmKind)
			reply := operation()
			require.Equal(t, "/zzrouter/v1/runs/a?node=worker", <-requests)
			if action == "delete" {
				require.Equal(t, "/zzrouter/v1/models", <-requests)
			}
			m.confirmKind, m.confirmMessage = confirmStopModel, "A newer confirmation"
			_, cmd := m.Update(reply)
			require.NotNil(t, cmd)
			require.Equal(t, confirmStopModel, m.confirmKind, "an older completion cannot clear a newer confirmation")
			require.Contains(t, m.statusMsg, action+" completed")
			require.True(t, m.fetching)
			assert.Contains(t, m.View().Content, action+" completed", "detail renders operation feedback while refresh is pending")
			batch := cmd().(tea.BatchMsg)
			m.Update(batch[0]())
			require.Contains(t, m.statusMsg, action+" completed")
			require.Contains(t, m.statusMsg, "Refresh failed")
			assert.Contains(t, m.View().Content, "Refresh failed", "detail renders a failed authoritative refresh")
			failReads.Store(false)
			m.Update(m.fetchAll()())
			require.Len(t, m.rows, 1)
			require.Equal(t, "B", m.rows[0].RawModel)
			require.Equal(t, modelsViewList, m.mode)
			require.Equal(t, confirmNone, m.confirmKind)
			m.rebuildDisplayRows()
			require.Len(t, m.rows, 1, "stopped/deleted rows cannot reappear after projection rebuild")
			assert.Contains(t, m.statusMsg, action+" completed")
		})
	}
}

func TestModelsEmptyStateGuidanceFitsNarrowTerminal(t *testing.T) {
	styles := ui.NewStyles(ui.CatppuccinMocha())
	for _, tc := range []struct {
		name string
		view *ModelsViewModel
		want []string
	}{
		{name: "available", view: NewModelsViewModel(nil, styles), want: []string{"Providers", "Discover"}},
		{name: "running", view: NewRunningModelsViewModel(nil, styles), want: []string{"No instances are running", "Available"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.view
			t.Cleanup(m.Cancel)
			loadModelRows(m, nil, nil)
			m.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
			frame := m.View().Content
			for _, text := range tc.want {
				require.Contains(t, frame, text)
			}
			require.LessOrEqual(t, lipgloss.Width(frame), 42)
			require.LessOrEqual(t, lipgloss.Height(frame), 18)
		})
	}
}
