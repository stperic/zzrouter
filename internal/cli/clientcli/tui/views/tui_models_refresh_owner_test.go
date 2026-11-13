package views

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsNewRouteDraftSurvivesBackgroundRowRemoval(t *testing.T) {
	for _, download := range []bool{false, true} {
		m := NewModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
		t.Cleanup(m.Cancel)
		row := instanceRow("selected", "run", "running")
		m.runningOnly = !download
		if download {
			row = modelRow{RawModel: "selected", Node: "worker", Provider: shared.EmptyValue,
				deploy: &shared.DeploymentInfo{DownloadID: "download", Model: "selected", Node: "worker"}}
		}
		loadModelRows(m, []modelRow{row}, nil)
		m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
		require.Equal(t, modelsViewNewRoute, m.mode)
		for _, ch := range "draft" {
			m.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		}
		require.Equal(t, "draft", m.routeForm.formName.Value())
		if download {
			m.Update(deploymentsRefreshedMsg{ctx: m.beginLoad()})
		} else {
			loadModelRows(m, nil, nil)
		}
		assert.Equal(t, modelsViewNewRoute, m.mode, "a new route does not target the removed background row")
		assert.Equal(t, "draft", m.routeForm.formName.Value())
		assert.True(t, m.routeForm.formName.Focused())
	}
}

func TestModelsRouteModesKeepTheirNamedTargetWhenRunningFilterHidesIt(t *testing.T) {
	for _, mode := range []modelsViewMode{modelsViewRouteDetail, modelsViewEditRoute, modelsViewAddDeploy} {
		m := NewRunningModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
		t.Cleanup(m.Cancel)
		a, b := instanceRow("A", "a", "running"), instanceRow("B", "b", "running")
		groups := []pkgClient.ModelGroupResponse{
			{Name: "target", Replicas: []pkgClient.ReplicaResponse{{Name: "target-replica", Model: "A", Node: "worker", App: "engine"}}},
			{Name: "other", Replicas: []pkgClient.ReplicaResponse{{Name: "other-replica", Model: "B", Node: "worker", App: "engine"}}},
		}
		loadModelRows(m, []modelRow{a, b}, groups)
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		require.Equal(t, modelsViewRouteDetail, m.mode)
		if mode == modelsViewEditRoute {
			m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
			m.routeForm.formDesc.SetValue("unsaved description")
		} else if mode == modelsViewAddDeploy {
			m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
			m.routeForm.deployTags.SetValue("unsaved, tags")
		}
		require.Equal(t, mode, m.mode)
		loadModelRows(m, []modelRow{b}, groups)
		require.Equal(t, mode, m.mode, "a configured route outlives its last running instance")
		target := m.selectedRouteGroup()
		require.NotNil(t, target)
		assert.Equal(t, "target", target.Name, "an editor must not retarget to the first visible route")
		if mode == modelsViewEditRoute {
			name, req := m.routeForm.buildGroupRequest(target)
			require.NotNil(t, req)
			assert.Equal(t, "target", name)
			assert.Equal(t, "unsaved description", req.Description)
			require.Len(t, req.Replicas, 1)
			assert.Equal(t, "target-replica", req.Replicas[0].Name)
		} else if mode == modelsViewAddDeploy {
			assert.Equal(t, "unsaved, tags", m.routeForm.deployTags.Value())
		}
		loadModelRows(m, []modelRow{b}, groups[1:])
		assert.Equal(t, modelsViewList, m.mode, "removing the actual named route retires its editor")
		assert.Equal(t, confirmNone, m.confirmKind)
	}
}

func TestModelsAvailableRouteShowsDownloadProgressWithoutARun(t *testing.T) {
	styles := ui.NewStyles(ui.CatppuccinMocha())
	pull := shared.DeploymentInfo{DownloadID: "download", Model: "weights", Node: "worker", Status: constants.StatusDownloading, Progress: "25%"}
	row := modelRow{RawModel: pull.Model, Node: pull.Node, Provider: shared.EmptyValue, Status: formatDeploymentStatus(&pull), deploy: &pull}
	groups := []pkgClient.ModelGroupResponse{{Name: "route", Replicas: []pkgClient.ReplicaResponse{{Name: "replica", Model: "weights", Node: "worker", App: "engine"}}}}
	m := NewModelsViewModel(nil, styles)
	t.Cleanup(m.Cancel)
	m.expandedRoutes["route"] = true
	loadModelRows(m, []modelRow{row}, groups)
	require.Len(t, m.rows, 3)
	child := m.rows[1]
	assert.Equal(t, row.Status, child.Status)
	assert.Equal(t, "engine", child.Provider)
	assert.Nil(t, child.instance)
	assert.False(t, child.isRunning())
	pull.Progress = "75%"
	m.updateDeploymentRows([]shared.DeploymentInfo{pull})
	assert.Contains(t, m.rows[1].Status, "75%")
	assert.Nil(t, m.rows[1].instance)
	running := NewRunningModelsViewModel(nil, styles)
	t.Cleanup(running.Cancel)
	running.expandedRoutes["route"] = true
	loadModelRows(running, m.rawModelRows, groups)
	assert.Empty(t, running.rows, "download progress cannot make a route or model running")
}

func TestModelsHiddenRouteWritesKeepTheirNamedTarget(t *testing.T) {
	for _, action := range []string{"edit", "add", "delete"} {
		t.Run(action, func(t *testing.T) {
			type mutation struct {
				method, path string
				body         pkgClient.CreateOrUpdateModelGroupRequest
			}
			writes := make(chan mutation, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				change := mutation{method: r.Method, path: r.URL.Path}
				if r.Method == http.MethodPut {
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&change.body))
				}
				writes <- change
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": pkgClient.ModelGroupResponse{Name: "target"}}))
			}))
			defer server.Close()
			m := NewRunningModelsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			a, b := instanceRow("A", "a", "running"), instanceRow("B", "b", "running")
			groups := []pkgClient.ModelGroupResponse{
				{Name: "target", Replicas: []pkgClient.ReplicaResponse{{Name: "target-replica", Model: "A", Node: "worker", App: "engine"}}},
				{Name: "other", Replicas: []pkgClient.ReplicaResponse{{Name: "other-replica", Model: "B", Node: "worker", App: "engine"}}},
			}
			loadModelRows(m, []modelRow{a, b}, groups)
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if action == "edit" {
				m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
				m.routeForm.formDesc.SetValue("edited target")
			} else if action == "add" {
				m.routeForm.availableModels = []pkgClient.ModelMetadata{{Name: "new-model", Node: "worker", AssignedApp: "engine"}}
				m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
				m.routeForm.deployTags.SetValue("draft")
			}
			loadModelRows(m, []modelRow{b}, groups)
			require.Equal(t, "other", m.selectedRow().RawModel, "the background table now points elsewhere")
			var cmd tea.Cmd
			if action == "delete" {
				m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
				_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			} else {
				_, cmd = m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			}
			require.NotNil(t, cmd)
			switch reply := cmd().(type) {
			case routesSavedMsg:
				require.NoError(t, reply.err)
			case routesDeletedMsg:
				require.NoError(t, reply.err)
			default:
				t.Fatalf("unexpected mutation response %T", reply)
			}
			change := <-writes
			assert.Equal(t, "/zzrouter/v1/model-groups/target", change.path)
			if action == "delete" {
				assert.Equal(t, http.MethodDelete, change.method)
			} else {
				assert.Equal(t, http.MethodPut, change.method)
				require.NotEmpty(t, change.body.Replicas)
				assert.Equal(t, "target-replica", change.body.Replicas[0].Name)
				if action == "add" {
					require.Len(t, change.body.Replicas, 2)
					assert.Equal(t, "new-model", change.body.Replicas[1].Name)
					assert.Equal(t, []string{"draft"}, change.body.Replicas[1].Tags)
				} else {
					assert.Equal(t, "edited target", change.body.Description)
				}
			}
		})
	}
}

func TestModelsDownloadRetryRetiresPendingCancel(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "standalone"
		if child {
			name = "route-child"
		}
		t.Run(name, func(t *testing.T) {
			writes := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes <- r.Method + " " + r.URL.Path
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()
			m := NewModelsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			pull := shared.DeploymentInfo{DownloadID: "worker/huggingface/weights", JobID: "old-attempt", Model: "weights", Node: "worker", Status: constants.StatusDownloading, Progress: "25%"}
			row := modelRow{RawModel: pull.Model, Node: pull.Node, Provider: shared.EmptyValue, Status: formatDeploymentStatus(&pull), deploy: &pull}
			var groups []pkgClient.ModelGroupResponse
			if child {
				groups = []pkgClient.ModelGroupResponse{{Name: "route", Replicas: []pkgClient.ReplicaResponse{{Name: "replica", Model: "weights", Node: "worker", App: "engine"}}}}
				m.expandedRoutes["route"] = true
			}
			loadModelRows(m, []modelRow{row}, groups)
			if child {
				m.list.SetCursor(1)
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
			require.Equal(t, confirmDeleteDownload, m.confirmKind)
			progress := pull
			progress.Progress = "50%"
			m.Update(deploymentsRefreshedMsg{ctx: m.beginLoad(), pulls: []shared.DeploymentInfo{progress}})
			require.Equal(t, confirmDeleteDownload, m.confirmKind, "progress on the same job preserves its confirmation")
			retry := progress
			retry.JobID, retry.Progress = "new-attempt", "1%"
			m.Update(deploymentsRefreshedMsg{ctx: m.beginLoad(), pulls: []shared.DeploymentInfo{retry}})
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			if cmd != nil {
				cmd()
			}
			select {
			case mutation := <-writes:
				t.Fatalf("the old confirmation retargeted a new transfer attempt: %s", mutation)
			default:
			}
			assert.Equal(t, confirmNone, m.confirmKind)
			assert.Equal(t, modelsViewList, m.mode)
		})
	}
}

func TestModelsCreatedRouteOpensDetailOutsideRunningProjection(t *testing.T) {
	m := NewRunningModelsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	loadModelRows(m, nil, []pkgClient.ModelGroupResponse{})
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	created := pkgClient.ModelGroupResponse{Name: "new-route", Replicas: []pkgClient.ReplicaResponse{}}
	m.Update(routesSavedMsg{group: &created, fromMode: modelsViewNewRoute})
	loadModelRows(m, nil, []pkgClient.ModelGroupResponse{created})
	require.Empty(t, m.rows, "an inactive route remains excluded from the Running list")
	require.Equal(t, modelsViewRouteDetail, m.mode)
	require.NotNil(t, m.selectedRouteGroup())
	assert.Equal(t, "new-route", m.selectedRouteGroup().Name)
	m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	assert.Equal(t, modelsViewAddDeploy, m.mode, "creation can proceed to adding its first replica")
}
