package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stperic/zzrouter/test/e2e/harness"
	"github.com/stretchr/testify/require"
)

type pausedUpdates struct{}

func (pausedUpdates) Status(context.Context, string) (update.UpdateStatus, error) {
	return update.UpdateStatus{}, update.ErrNodeUnavailable
}
func (pausedUpdates) Apply(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected dispatch")
}

func installTestUpdateOwner(t *testing.T, s *Server) {
	t.Helper()
	disabled := false
	s.updateScheduler = update.NewSchedulerIn(t.TempDir(), &config.UpdateConfig{Enabled: &disabled}, clock.System(), update.WithJobsRegistry(s.jobs))
	t.Cleanup(s.updateScheduler.Stop)
	owner, err := update.NewRollouts(filepath.Join(t.TempDir(), "rollouts.json"), s.node.Name(), pausedUpdates{}, s.jobs, clock.System())
	require.NoError(t, err)
	s.updateRollouts = owner
	t.Cleanup(owner.Stop)
}

func TestIntegrationUpdateAPIRecordedAdminContract(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, NodeName: "coord", ClusterMode: config.ClusterModeCoordinator})
	installTestUpdateOwner(t, s)
	httpServer := httptest.NewServer(s.engine)
	defer httpServer.Close()
	node, err := harness.NewNode("coord", httpServer.URL, harness.RoleCoordinator, nil, harness.NodeKeys{Admin: TestAdminKey, API: TestUserKey})
	require.NoError(t, err)
	client := harness.NewClient(node, harness.TierAdmin)
	ctx := context.Background()
	if dir := os.Getenv("ZZROUTER_E2E_COVERAGE_DIR"); dir != "" {
		catalog, err := harness.FetchRoutes(ctx, node.HTTPClient(), httpServer.URL, TestAdminKey)
		require.NoError(t, err)
		data, err := json.MarshalIndent(catalog, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(dir), "catalog.json"), data, 0600))
	}
	for _, path := range []string{"/zzrouter/v1/update/status?nodes=all", "/zzrouter/v1/update/history?nodes=all", "/zzrouter/v1/update/settings"} {
		response, err := client.GET(ctx, path)
		require.NoError(t, err)
		require.Equal(t, 200, response.Status, string(response.Body))
	}
	for _, path := range []string{"/zzrouter/v1/update/status?nodes=missing", "/zzrouter/v1/update/history?nodes=missing", "/zzrouter/v1/update/settings?node=missing"} {
		response, err := client.GET(ctx, path)
		require.NoError(t, err)
		require.Equal(t, 400, response.Status, string(response.Body))
	}
	response, err := client.PATCH(ctx, "/zzrouter/v1/update/settings", map[string]any{"enabled": false})
	require.NoError(t, err)
	require.Equal(t, 200, response.Status, string(response.Body))
	require.False(t, s.updateScheduler.Enabled())
	require.False(t, s.nodeConfigStore.Config().Update.IsEnabled())
	for _, bad := range []map[string]any{{}, {"enabled": nil}, {"enabled": "false"}, {"enabled": false, "source": "evil"}} {
		response, err := client.PATCH(ctx, "/zzrouter/v1/update/settings", bad)
		require.NoError(t, err)
		require.Equal(t, 400, response.Status, string(response.Body))
	}
	for _, bad := range []map[string]any{{"version": "https://evil"}, {"version": ""}, {"version": "1.2.3", "nodes": []string{}}, {"version": "1.2.3", "nodes": []string{"missing"}}, {"version": "1.2.3", "url": "https://evil"}, {"nodes": []string{"all"}}} {
		response, err := client.POST(ctx, "/zzrouter/v1/update/apply", bad)
		require.NoError(t, err)
		require.Equal(t, 400, response.Status, string(response.Body))
	}
	for _, tier := range []harness.StaticTier{harness.TierNone, harness.TierAPI} {
		restricted := harness.NewClient(node, tier)
		response, err := restricted.PATCH(ctx, "/zzrouter/v1/update/settings", map[string]any{"enabled": true})
		require.NoError(t, err)
		require.Contains(t, []int{401, 403}, response.Status)
		response, err = restricted.POST(ctx, "/zzrouter/v1/update/apply", map[string]any{"version": "2.0.0", "nodes": []string{"all"}})
		require.NoError(t, err)
		require.Contains(t, []int{401, 403}, response.Status)
	}
	response, err = client.POST(ctx, "/zzrouter/v1/update/apply", map[string]any{"version": "2.0.0-lab.1", "nodes": []string{"all"}})
	require.NoError(t, err)
	require.Equal(t, 202, response.Status, string(response.Body))
	var accepted struct {
		Data update.Rollout `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &accepted))
	require.NotEmpty(t, accepted.Data.JobID)
	require.Len(t, accepted.Data.Nodes, 1)
	response, err = client.POST(ctx, "/zzrouter/v1/update/apply", map[string]any{"version": "2.0.0", "nodes": []string{"all"}})
	require.NoError(t, err)
	require.Equal(t, 409, response.Status)
	response, err = client.DELETE(ctx, "/zzrouter/v1/jobs/"+accepted.Data.JobID)
	require.NoError(t, err)
	require.Equal(t, 202, response.Status, string(response.Body))
	require.Equal(t, "cancelled", s.updateRollouts.History()[0].State)
	require.NoError(t, harness.WriteCoverageLedger("update_control_inprocess"))
}

func TestIntegrationUpdateWorkerRouteBoundary(t *testing.T) {
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: config.ClusterModeWorker})
	installTestUpdateOwner(t, worker)
	path := "/zzrouter/v1/internal/update/settings"
	public := makeAuthRequest(t, worker, http.MethodPatch, path, TestAdminKey, map[string]any{"enabled": true})
	require.Equal(t, 404, public.Code, string(public.Body))
	require.False(t, worker.updateScheduler.Enabled())
	result := makeInternalRequest(t, worker, TestRequest{Method: http.MethodPatch, Path: path, Body: map[string]any{"enabled": false}})
	require.Equal(t, 200, result.Code, string(result.Body))
	require.False(t, worker.nodeConfigStore.Config().Update.IsEnabled())
	result = makeInternalRequest(t, worker, TestRequest{Method: http.MethodPost, Path: "/zzrouter/v1/internal/update/apply", Body: map[string]any{"version": "2.0.0", "url": "evil"}})
	require.Equal(t, 400, result.Code, string(result.Body))
	coord := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: config.ClusterModeCoordinator})
	out := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/update/apply", bytes.NewBufferString(`{"version":"2.0.0"}`))
	buildCoordInternalEngine(coord).ServeHTTP(out, request)
	require.Equal(t, 404, out.Code)
}

type updateWireClient struct {
	mesh.ClusterClient
	engine      http.Handler
	requests    []string
	body        []byte
	unavailable bool
}

func (f *updateWireClient) Unicast(ctx context.Context, _ string, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	if f.unavailable {
		return nil, errors.New("node asleep")
	}
	f.requests = append(f.requests, path)
	f.body = params.Body
	request, err := http.NewRequestWithContext(ctx, params.Method, path, bytes.NewReader(params.Body))
	if err != nil {
		return nil, err
	}
	out := httptest.NewRecorder()
	f.engine.ServeHTTP(out, request)
	return &mesh.Response{StatusCode: out.Code, Body: out.Body.Bytes()}, nil
}
func TestUpdateWireVersionOnlyAndUnsupportedPeer(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(strconv.FormatBool(supported), func(t *testing.T) {
			var capabilities []string
			if supported {
				capabilities = []string{version.CapabilityClusterUpdates}
			}
			engine := gin.New()
			engine.GET("/zzrouter/v1/internal/version", func(c *gin.Context) {
				c.JSON(200, version.VersionInfo{ClusterProtocol: version.ClusterProtocolVersion, Capabilities: capabilities})
			})
			engine.POST("/zzrouter/v1/internal/update/apply", func(c *gin.Context) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(c.Request.Body).Decode(&body))
				require.Equal(t, map[string]any{"version": "2.0.0-lab.1"}, body)
				c.JSON(202, gin.H{"operation_id": "worker-operation"})
			})
			client := &updateWireClient{engine: engine}
			var result struct {
				OperationID string `json:"operation_id"`
			}
			err := queryUpdateNode(context.Background(), client, "worker", "/update/apply", http.MethodPost, []byte(`{"version":"2.0.0-lab.1"}`), &result)
			if !supported {
				require.ErrorContains(t, err, "does not support cluster updates")
				require.Len(t, client.requests, 1)
			} else {
				require.NoError(t, err)
				require.Equal(t, "worker-operation", result.OperationID)
				require.Len(t, client.requests, 2)
			}
			client.unavailable = true
			require.ErrorIs(t, queryUpdateNode(context.Background(), client, "worker", "/update/status", http.MethodGet, nil, &result), update.ErrNodeUnavailable)
		})
	}
}
func TestUpdateAllIncludesCachedSleepingPeer(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: config.ClusterModeCoordinator})
	require.NoError(t, s.nodeConfigStore.AddClusterEndpoint("127.0.0.1:19999"))
	_, err := s.nodeConfigStore.SetClusterEndpointName("127.0.0.1:19999", "sleeping")
	require.NoError(t, err)
	require.NoError(t, s.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{URL: "http://127.0.0.1:19999", Name: "http://127.0.0.1:19999", Status: mesh.StatusDown}))
	nodes, err := s.selectUpdateNodes([]string{"all"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"coord", "sleeping"}, nodes)
	nodes, err = s.selectUpdateNodes([]string{"sleeping"})
	require.NoError(t, err)
	require.Equal(t, []string{"sleeping"}, nodes)
	_, err = s.selectUpdateNodes([]string{"coord", "coord"})
	require.Error(t, err)
}

func TestUpdateSettingsDelayedNotificationUsesLatestState(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: config.ClusterModeCoordinator})
	installTestUpdateOwner(t, s)
	entered, release := make(chan struct{}), make(chan struct{})
	s.nodeConfigStore.OnChange("updateScheduling", func(cfg *config.NodeConfig) config.ReloadDisposition {
		if !cfg.Update.IsEnabled() {
			close(entered)
			<-release
		}
		return s.reconcileUpdateScheduling(cfg)
	})
	first := make(chan *TestResponse, 1)
	go func() {
		first <- makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/update/settings", TestAdminKey, map[string]any{"enabled": false})
	}()
	<-entered
	second := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/update/settings", TestAdminKey, map[string]any{"enabled": true})
	require.Equal(t, 200, second.Code, string(second.Body))
	close(release)
	response := <-first
	require.Equal(t, 200, response.Code, string(response.Body))
	require.True(t, s.nodeConfigStore.Config().Update.IsEnabled())
	require.True(t, s.updateScheduler.Enabled(), "stale notification reversed the persisted setting")
}

func TestIntegrationUpdateRemoteWorkerOwnsSettingsAndRelease(t *testing.T) {
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: config.ClusterModeWorker})
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/stperic/zzrouter/releases/tags/v2.0.0-lab.1", r.URL.Path)
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0-lab.1","prerelease":true}`))
	}))
	defer release.Close()
	disabled := false
	worker.updateScheduler = update.NewSchedulerIn(t.TempDir(), &config.UpdateConfig{Enabled: &disabled, Source: &config.UpdateSourceConfig{Owner: "stperic", Repo: "zzrouter", APIBaseURL: release.URL}}, clock.System())
	defer worker.updateScheduler.Stop()
	engine := buildInternalEngine(worker)
	// The connector is real; this fixture substitutes the separately-tested certificate gate.
	commands := make(chan map[string]any, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/zzrouter/v1/internal/update/apply" {
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var command map[string]any
			require.NoError(t, json.Unmarshal(data, &command))
			commands <- command
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		engine.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	coord := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: config.ClusterModeCoordinator})
	installTestUpdateOwner(t, coord)
	connector := mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: upstream.Client()})
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coord"}, "http://coord.invalid", coord, nil, connector)
	require.NoError(t, err)
	coord.cluster.coordinator = cluster
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: upstream.URL, ClusterURL: upstream.URL, NodeName: "worker", Status: mesh.StatusUp}))
	response := makeAuthRequest(t, coord, http.MethodPatch, "/zzrouter/v1/update/settings?node=worker", TestAdminKey, map[string]any{"enabled": true})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.True(t, worker.nodeConfigStore.Config().Update.IsEnabled())
	require.True(t, worker.updateScheduler.Enabled())
	require.False(t, coord.updateScheduler.Enabled())
	var setting struct {
		Data struct {
			Node    string `json:"node"`
			Enabled bool   `json:"enabled"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &setting))
	require.Equal(t, "worker", setting.Data.Node)
	require.True(t, setting.Data.Enabled)
	response = makeAuthRequest(t, coord, http.MethodPatch, "/zzrouter/v1/update/settings?node=worker", TestAdminKey, map[string]any{"enabled": false})
	require.Equal(t, 200, response.Code, string(response.Body))
	id, err := (clusterUpdates{coord}).Apply(context.Background(), "worker", "2.0.0-lab.1")
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.Equal(t, map[string]any{"version": "2.0.0-lab.1"}, <-commands)
	require.Eventually(t, func() bool {
		status := worker.updateScheduler.GetStatus()
		return status.Operation != nil && status.Operation.Finished()
	}, time.Second, time.Millisecond)
	status := worker.updateScheduler.GetStatus()
	require.Equal(t, id, status.Operation.JobID)
	require.ErrorContains(t, errors.New(status.Operation.Error), "checksums.txt.sigstore.json")
	require.False(t, status.Operation.Success)
	require.Nil(t, coord.updateScheduler.GetStatus().Operation, "coordinator performed the worker's install")
}
