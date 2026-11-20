package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestIntegrationRouteStrategiesAcrossNodes(t *testing.T) {
	const alias = "qwen-route"
	providers := [2]string{"vllm", "mlx"}
	models := [2]string{"Qwen/Qwen3.8-27B", "mlx-community/Qwen3.8-27B-8bit"}
	nodes := []string{"coord", "worker-a", "worker-b"}
	request := map[string]any{"model": alias, "messages": []map[string]string{{"role": "user", "content": "hi"}}}

	type target struct{ provider, node string }
	var calls []target
	unavailable := target{}
	workers := map[string]http.Handler{}
	var catalog []map[string]any
	for _, node := range nodes {
		for i, provider := range providers {
			catalog = append(catalog, map[string]any{"name": models[i], "node": node, "assigned_app": provider})
		}
	}
	var coordinator *Server
	for _, node := range nodes {
		role := config.ClusterModeWorker
		if node == "coord" {
			role = config.ClusterModeCoordinator
		}
		server := createSeededTestNode(t, seededTestNodeConfig{
			TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: role, NodeName: node},
			providers:      providers[:],
			enabled:        providers[:],
		})
		routingCatalog(t, server, catalog)
		for i, provider := range providers {
			run := instance.NewInstance(node+"-"+provider, provider, models[i], 8100+i, 0, 0)
			run.WireModel = models[i]
			run.MarkRunning(false, nil)
			require.NoError(t, server.providers.appMgr.Instances().Register(run))
			t.Cleanup(func() { _ = server.providers.appMgr.Instances().Remove(run.ID) })
		}
		engine := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
			var i int
			switch r.URL.Port() {
			case "8100":
				i = 0
			case "8101":
				i = 1
			default:
				return nil, fmt.Errorf("unexpected engine port %s on %s", r.URL.Port(), node)
			}
			var body struct{ Model string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body.Model != models[i] {
				return nil, fmt.Errorf("engine on %s got model %q, expected %q", node, body.Model, models[i])
			}
			called := target{providers[i], node}
			calls = append(calls, called)
			status, response := http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`
			if called == unavailable {
				status, response = http.StatusServiceUnavailable, `{"error":{"message":"temporarily unavailable"}}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(response))}, nil
		})}
		server.httpStreamingClient = engine
		server.proxy.streamClient = engine
		if node == "coord" {
			coordinator = server
		} else {
			workers[node+".test:9091"] = buildWorkerCompatEngine(server)
			require.NoError(t, coordinator.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{
				Name: node, URL: "http://" + node + ".test:9090", ClusterURL: "https://" + node + ".test:9091",
			}))
			// Enough total memory for the running model, with little left for a second run.
			coordinator.observePeerSnapshot("http://"+node+".test:9090", &mesh.Connection{
				NodeName: node, Quality: mesh.QualityFull, HealthReport: mesh.HealthReport{
					VRAMTotalGB: 96, VRAMAvailableGB: 4778.0 / 1024,
				},
			})
		}
	}
	coordinator.clusterMTLSClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		worker := workers[r.URL.Host]
		if worker == nil {
			return nil, fmt.Errorf("unexpected worker %s", r.URL.Host)
		}
		w := httptest.NewRecorder()
		worker.ServeHTTP(w, r)
		return w.Result(), nil
	})}

	scenarios := []struct {
		name         string
		strategy     modelgroup.StrategyType
		busy, fast   int // One-based priority positions, zero means no observation.
		winner       int // One-based priority position.
		refuseWinner bool
	}{
		{name: "priority", strategy: modelgroup.StrategyPriority, busy: 1, fast: 2, winner: 1},
		{name: "priority fallback", strategy: modelgroup.StrategyPriority, busy: 1, fast: 2, winner: 1, refuseWinner: true},
		{name: "least load tie", strategy: modelgroup.StrategyLeastLoad, fast: 2, winner: 1},
		{name: "least load first", strategy: modelgroup.StrategyLeastLoad, busy: 2, fast: 2, winner: 1},
		{name: "least load second", strategy: modelgroup.StrategyLeastLoad, busy: 1, fast: 1, winner: 2},
		{name: "least load fallback", strategy: modelgroup.StrategyLeastLoad, busy: 1, fast: 1, winner: 2, refuseWinner: true},
		{name: "fastest no observations", strategy: modelgroup.StrategyFastest, busy: 1, winner: 1},
		{name: "fastest first", strategy: modelgroup.StrategyFastest, busy: 1, fast: 1, winner: 1},
		{name: "fastest second", strategy: modelgroup.StrategyFastest, busy: 2, fast: 2, winner: 2},
		{name: "fastest fallback", strategy: modelgroup.StrategyFastest, busy: 2, fast: 2, winner: 2, refuseWinner: true},
	}
	destinations := []struct{ label, node, servingNode string }{
		{"default-local", "", "coord"},
		{"coord", "coord", "coord"},
		{"worker-a", "worker-a", "worker-a"},
		{"worker-b", "worker-b", "worker-b"},
	}
	var caseNumber int
	for _, vllm := range destinations {
		for _, mlx := range destinations {
			for firstProvider, name := range providers {
				for _, scenario := range scenarios {
					t.Run(vllm.label+"/"+mlx.label+"/"+name+" first/"+scenario.name, func(t *testing.T) {
						caseNumber++
						targets := [2]target{{providers[0], vllm.servingNode}, {providers[1], mlx.servingNode}}
						nodeNames := [2]string{vllm.node, mlx.node}
						order := [2]int{firstProvider, 1 - firstProvider}
						var replicas [2]modelgroup.Replica
						for position, i := range order {
							replicas[position] = modelgroup.Replica{
								Name: fmt.Sprintf("%s-%d", providers[i], caseNumber), App: providers[i], Model: models[i], Node: nodeNames[i],
								Priority: position + 1, OnDemand: true, MaxRetries: new(int),
							}
							coordinator.providers.cooldowns.Providers().ClearCooldown(providers[i])
						}
						require.NoError(t, coordinator.model.Groups.Set(alias, modelgroup.ModelGroup{
							Strategy: scenario.strategy,
							Replicas: []modelgroup.Replica{replicas[1], replicas[0]},
						}))
						if scenario.busy > 0 {
							replica := replicas[scenario.busy-1].Name
							coordinator.providers.loadTracker.Acquire(replica)
							t.Cleanup(func() { coordinator.providers.loadTracker.Release(replica) })
						}
						if scenario.fast > 0 {
							coordinator.providers.latencyTracker.Record(replicas[scenario.fast-1].Name, time.Millisecond)
							coordinator.providers.latencyTracker.Record(replicas[2-scenario.fast].Name, time.Second)
						}
						calls, unavailable = nil, target{}
						selected := scenario.winner - 1
						want := []target{targets[order[selected]]}
						if scenario.refuseWinner {
							unavailable = want[0]
							want = append(want, targets[order[1-selected]])
						}
						response := makeAuthRequest(t, coordinator, http.MethodPost, "/v1/chat/completions", TestAdminKey, request)
						require.Equal(t, http.StatusOK, response.Code, string(response.Body))
						require.Equal(t, want, calls)
						winner := want[len(want)-1]
						require.Equal(t, winner.provider, response.Headers.Get(constants.HeaderServingProvider))
						require.Equal(t, winner.node, response.Headers.Get(constants.HeaderServingNode))
						require.Equal(t, string(scenario.strategy), response.Headers.Get("X-zzrouter-Strategy"))
					})
				}
			}
		}
	}
}
