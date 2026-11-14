package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestConsoleWorkerObservationContract(t *testing.T) {
	used, zero := int64(17<<30), int64(0)
	sample := inventory.NewMemorySample(64<<30, &used, time.Now().Add(-12*time.Second))
	sample.ObservedAt = time.Now().Add(24 * time.Hour) // receiver must ignore peer clock
	activity := instance.Activity{State: "active", ActiveRequests: 1, ObservedAt: sample.ObservedAt, AgeSeconds: 2, ValidForSeconds: 20}
	service := &prov_apps.ProviderServiceStatus{Provider: "engine", Running: false, Backoff: "waiting", JobID: "ready-job"}
	provider := prov_apps.LocalProviderInfo{Key: "engine", Name: "engine", State: prov_apps.StateLive, RunLifecycle: "process", RunScope: "node", Service: service}
	data := map[string]any{
		"hostname": "worker", "memory_total": map[string]any{"value": int64(64 << 30)}, "memory_available": map[string]any{"value": int64(32 << 30)},
		"memory_sample": sample, "activity": activity, "provider_activity": map[string]any{"engine": activity}, "apps_detail": []prov_apps.LocalProviderInfo{provider},
		"gpu_count": 2, "gpu_type": "nvidia", "gpus": []map[string]any{
			{"name": "A", "uuid": "A", "vram_total": map[string]any{"value": int64(24 << 30)}, "memory_sample": inventory.NewMemorySample(24<<30, &zero, time.Now())},
			{"name": "B", "uuid": "B", "vram_total": map[string]any{"value": int64(24 << 30)}, "memory_sample": inventory.NewMemorySample(24<<30, nil, time.Now())},
		},
	}
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zzrouter/v1/internal/version" {
			info := version.GetCurrentVersionInfo(version.ServiceTypeHost, version.ClusterRequiredCapabilities)
			info.Version = version.MustParseVersion("0.1.0")
			require.NoError(t, json.NewEncoder(w).Encode(info))
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	defer peer.Close()
	address, err := url.Parse(peer.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(address.Port())
	require.NoError(t, err)
	connector := mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: peer.Client(), ClusterPort: port})
	before := time.Now()
	connection, err := connector.ConnectToClusterNode(t.Context(), peer.URL)
	require.NoError(t, err)
	ep := &mesh.Endpoint{URL: peer.URL, NodeName: "worker", Status: mesh.StatusUp, Snapshot: mesh.EndpointSnapshot{HealthReport: connection.HealthReport}}
	view := (&NodesService{}).endpointToNodeInfo(ep)
	memory := view["memory"].(map[string]any)["sample"].(*inventory.MemorySample)
	require.Equal(t, used, *memory.UsedBytes, "must not replace measured 17 GiB with total minus available (32 GiB)")
	require.True(t, memory.ObservedAt.Before(before))
	require.GreaterOrEqual(t, memory.AgeSeconds, float64(12))
	require.False(t, memory.Stale)
	require.Equal(t, "active", view["activity"].(*instance.Activity).State)
	require.LessOrEqual(t, view["activity"].(*instance.Activity).ValidForSeconds, float64(20))
	cards := view["gpu"].(map[string]any)["gpus"].([]map[string]any)
	require.Zero(t, *cards[0]["sample"].(*inventory.MemorySample).UsedBytes)
	require.Nil(t, cards[1]["sample"].(*inventory.MemorySample).UsedBytes)
	public := appInfoFromLocal(connection.Apps[0], "worker")
	require.Equal(t, "process", public.RunLifecycle)
	require.Equal(t, "ready-job", public.Service.JobID)
	public.Service.Backoff = "changed"
	require.Equal(t, "waiting", connection.Apps[0].Service.Backoff)
	ep.Status = mesh.StatusDown
	down := (&NodesService{}).endpointToNodeInfo(ep)
	require.True(t, down["memory"].(map[string]any)["sample"].(*inventory.MemorySample).Stale)
	require.Equal(t, "unknown", down["activity"].(*instance.Activity).State)
	require.False(t, connection.MemorySample.Stale, "projection must not mutate the retained snapshot")
}

func TestConsoleLocalHealthAndNodeShareObservations(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	run := instance.NewInstance("console-activity", "fixture", "model", 0, 0, 10)
	run.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	lease, err := run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	defer lease.Release()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/health", nil)
	s.handleHealth(ctx)
	var health struct {
		Data struct {
			Memory   *inventory.MemorySample `json:"memory_sample"`
			Activity instance.Activity       `json:"activity"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &health))
	local := s.GetLocalNodeInfo()
	memory := local["memory"].(*inventory.NodeMemory).Sample
	require.NotNil(t, health.Data.Memory)
	require.Equal(t, health.Data.Memory.UsedBytes, memory.UsedBytes)
	require.Equal(t, health.Data.Memory.ObservedAt, memory.ObservedAt)
	require.Equal(t, "active", health.Data.Activity.State)
	require.Equal(t, "active", local["activity"].(instance.Activity).State)
}
