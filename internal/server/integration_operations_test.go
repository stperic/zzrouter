package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/config"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func operationTestNode(t *testing.T, node string, mode config.ClusterMode) *Server {
	t.Helper()
	return createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, NodeName: node, ClusterMode: mode}, providers: []string{"llamacpp", "vllm"}, enabled: []string{"llamacpp", "vllm"}})
}

// These synthetic ready runs exercise exact-instance HTTP admission without starting an engine.
func seedOperationRun(t *testing.T, s *Server, provider, model, endpoint string) *instance.Instance {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	run := instance.NewInstance(provider+"-fixture", provider, model, port, time.Minute, 1)
	run.BaseURL = "http://advertised.invalid:1"
	run.Config = instance.Config{Provider: provider, Model: model, LaunchMode: instance.LaunchModeNative}
	run.SetResolved(instance.Resolved{Runtime: provider, Listener: &instance.ListenerInfo{URL: endpoint, Host: "127.0.0.1", Scope: "loopback", Authentication: "unknown", Source: "launch_configuration"}})
	run.SetStatus(instance.StatusRunning)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { run.Cancel(); _ = s.providers.appMgr.Instances().Remove(run.ID) })
	return run
}

func operationTestRequest(t *testing.T, s *Server, run *instance.Instance) prov_apps.OperationRequest {
	t.Helper()
	operation, ok := s.appsConfig().Operation(run.Provider + ".tokenize")
	require.True(t, ok)
	info := run.ToInfo(s.node.Name())
	return prov_apps.OperationRequest{Version: operation.Definition().Version, DefinitionRevision: operation.Revision(), Target: prov_apps.OperationTarget{Provider: info.Provider, Node: info.Node, RunID: info.ID, Model: info.Model, Runtime: info.Runtime}, Preconditions: prov_apps.OperationPreconditions{TargetRevision: info.TargetRevision}, Input: map[string]any{"content": "hello"}}
}

func TestIntegrationProviderOperationsExactRunAndConfigOnlyBindings(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/tokenize", r.URL.Path)
		for _, name := range []string{"Authorization", "X-API-Key", "X-Cluster-API-Key"} {
			assert.Empty(t, r.Header.Get(name))
		}
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		assert.NotEmpty(t, input)
		_, _ = w.Write([]byte(`{"tokens":[1,9007199254740993],"count":2,"max_model_len":4096}`))
	}))
	defer upstream.Close()
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	before := []string{}
	for _, route := range s.engine.Routes() {
		before = append(before, route.Method+" "+route.Path)
	}
	for _, provider := range []string{"llamacpp", "vllm"} {
		t.Run(provider, func(t *testing.T) {
			run := seedOperationRun(t, s, provider, "same-model", upstream.URL)
			request := operationTestRequest(t, s, run)
			if provider == "vllm" {
				request.Input = map[string]any{"prompt": "hello", "return_token_strs": true}
			}
			result := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/operations/"+provider+".tokenize/execute", TestAdminKey, request)
			require.Equal(t, http.StatusOK, result.Code, string(result.Body))
			require.Contains(t, string(result.Body), "9007199254740993")
			var response struct {
				Data prov_apps.OperationResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(result.Body, &response))
			assert.Equal(t, request.Target, response.Data.Target)
			assert.Equal(t, request.DefinitionRevision, response.Data.DefinitionRevision)
			assert.Zero(t, run.GetActiveRequests())
		})
	}
	assert.EqualValues(t, 2, calls.Load())
	after := []string{}
	for _, route := range s.engine.Routes() {
		after = append(after, route.Method+" "+route.Path)
	}
	require.Equal(t, before, after, "provider bindings do not add public routes")
	for _, test := range []struct {
		path, key string
		status    int
		expected  string
	}{
		{"/zzrouter/v1/operations", TestAdminKey, 200, "llamacpp.tokenize"},
		{"/inference/v1/operations", TestUserKey, 200, `"data":[]`},
		{"/inference/v1/operations/llamacpp.tokenize", TestUserKey, 404, "operation_not_found"},
		{"/zzrouter/v1/operations", TestUserKey, 403, ""},
		{"/inference/v1/operations", TestAdminKey, 403, ""},
		{"/zzrouter/v1/operations/llamacpp.tokenize", TestAdminKey, 200, "input_schema"},
	} {
		response := makeAuthRequest(t, s, http.MethodGet, test.path, test.key, nil)
		assert.Equal(t, test.status, response.Code, string(response.Body))
		assert.Contains(t, string(response.Body), test.expected)
	}
}

func TestIntegrationProviderOperationRefusalsDoNotDispatch(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"tokens":[1]}`)) }))
	defer upstream.Close()
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	run := seedOperationRun(t, s, "llamacpp", "model", upstream.URL)
	base := operationTestRequest(t, s, run)
	for _, test := range []struct {
		name, code string
		change     func(*prov_apps.OperationRequest)
	}{
		{"definition", "operation_definition_stale", func(r *prov_apps.OperationRequest) { r.DefinitionRevision = "old" }},
		{"version", "operation_definition_stale", func(r *prov_apps.OperationRequest) { r.Version++ }},
		{"revision", "operation_target_stale", func(r *prov_apps.OperationRequest) { r.Preconditions.TargetRevision = "old" }},
		{"node alias", "operation_target_mismatch", func(r *prov_apps.OperationRequest) { r.Target.Node = "localhost" }},
		{"run", "operation_target_unavailable", func(r *prov_apps.OperationRequest) { r.Target.RunID = "absent" }},
		{"model", "operation_target_mismatch", func(r *prov_apps.OperationRequest) { r.Target.Model = "other" }},
		{"runtime", "operation_target_mismatch", func(r *prov_apps.OperationRequest) { r.Target.Runtime = "other" }},
		{"input", "operation_input_invalid", func(r *prov_apps.OperationRequest) { r.Input = map[string]any{"content": []any{"bad"}} }},
		{"extra input", "operation_input_invalid", func(r *prov_apps.OperationRequest) {
			r.Input = map[string]any{"content": "valid", "url": "http://other.invalid"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.change(&request)
			response := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, request)
			require.Contains(t, string(response.Body), test.code)
			require.GreaterOrEqual(t, response.Code, 400)
		})
	}
	for _, body := range []string{`{"version":1,"url":"http://other.invalid"}`, `{}`, `null`, `{} {}`, `{"target":{"url":"http://other.invalid"}}`} {
		request := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", strings.NewReader(body))
		request.Header.Set("X-API-Key", TestAdminKey)
		response := httptest.NewRecorder()
		s.engine.ServeHTTP(response, request)
		assert.Equal(t, 400, response.Code, response.Body.String())
	}
	run.SetStatus(instance.StatusStopped)
	response := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, base)
	assert.Contains(t, string(response.Body), "operation_target_unavailable")
	assert.Zero(t, calls.Load())
}

func TestIntegrationProviderOperationUpstreamAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name, output, code string
		status             int
	}{
		{"invalid JSON", `no`, "operation_upstream_invalid", 200},
		{"trailing JSON", `{"tokens":[1]} {}`, "operation_upstream_invalid", 200},
		{"output shape", `{"tokens":"bad"}`, "operation_output_invalid", 200},
		{"bounded", strings.Repeat(" ", 1<<20) + `{}`, "operation_upstream_invalid", 200},
		{"redirect", `{"tokens":[1]}`, "operation_upstream_invalid", 302},
	} {
		t.Run(test.name, func(t *testing.T) {
			var redirectCalls atomic.Int32
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectCalls.Add(1) }))
			defer redirect.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", redirect.URL)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.output))
			}))
			defer upstream.Close()
			s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
			run := seedOperationRun(t, s, "llamacpp", "model", upstream.URL)
			response := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, operationTestRequest(t, s, run))
			assert.Equal(t, 502, response.Code, string(response.Body))
			assert.Contains(t, string(response.Body), test.code)
			assert.Zero(t, run.GetActiveRequests())
			assert.Zero(t, redirectCalls.Load())
		})
	}
	t.Run("cancellation releases lease", func(t *testing.T) {
		started, stopped := make(chan struct{}), make(chan struct{})
		release := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			close(stopped)
		}))
		defer upstream.Close()
		defer close(release)
		s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
		run := seedOperationRun(t, s, "llamacpp", "model", upstream.URL)
		body, err := json.Marshal(operationTestRequest(t, s, run))
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", bytes.NewReader(body)).WithContext(ctx)
		request.Header.Set("X-API-Key", TestAdminKey)
		done := make(chan struct{})
		go func() { defer close(done); s.engine.ServeHTTP(httptest.NewRecorder(), request) }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream did not start")
		}
		require.EqualValues(t, 1, run.GetActiveRequests())
		cancel()
		for _, signal := range []<-chan struct{}{done, stopped} {
			select {
			case <-signal:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled operation did not drain")
			}
		}
		require.Zero(t, run.GetActiveRequests())
	})
}

func TestIntegrationProviderOperationWorkerMTLSExactTargetAndRevision(t *testing.T) {
	var localCalls, workerCalls atomic.Int32
	localBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls.Add(1)
		_, _ = w.Write([]byte(`{"tokens":[11]}`))
	}))
	defer localBackend.Close()
	workerBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerCalls.Add(1)
		_, _ = w.Write([]byte(`{"tokens":[22]}`))
	}))
	defer workerBackend.Close()
	worker := operationTestNode(t, "worker", config.ClusterModeWorker)
	coordinator := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	localRun := seedOperationRun(t, coordinator, "llamacpp", "same-model", localBackend.URL)
	remoteRun := seedOperationRun(t, worker, "llamacpp", "same-model", workerBackend.URL)
	linkOperationWorker(t, coordinator, worker)
	request := operationTestRequest(t, worker, remoteRun)
	response := makeAuthRequest(t, coordinator, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, request)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), `"tokens":[22]`)
	require.Zero(t, localCalls.Load())
	require.EqualValues(t, 1, workerCalls.Load())
	require.Zero(t, remoteRun.GetActiveRequests())
	require.Zero(t, localRun.GetActiveRequests())
	connections := makeAuthRequest(t, coordinator, http.MethodGet, "/zzrouter/v1/connections?node=worker", TestAdminKey, nil)
	require.Equal(t, 200, connections.Code, string(connections.Body))
	require.Contains(t, string(connections.Body), workerBackend.URL)
	require.NotContains(t, string(connections.Body), localBackend.URL)
	blocked := makeAuthRequest(t, worker, http.MethodPost, "/zzrouter/v1/internal/operations/llamacpp.tokenize/execute", TestAdminKey, request)
	require.Equal(t, 404, blocked.Code, "worker admin port never admits internal requests")
	require.NoError(t, worker.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		op := svc.Operations["tokenize"]
		op.Description = "Changed diagnostic"
		svc.Operations["tokenize"] = op
		return nil
	}))
	response = makeAuthRequest(t, coordinator, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, request)
	require.Equal(t, 409, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), "operation_definition_stale")
	require.EqualValues(t, 1, workerCalls.Load())
	definition, ok := coordinator.appsConfig().Operation("llamacpp.tokenize")
	require.True(t, ok)
	require.NoError(t, worker.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		op := svc.Operations["tokenize"]
		op.Description = definition.Definition().Description
		svc.Operations["tokenize"] = op
		return nil
	}))
	remoteRun.Cancel()
	require.NoError(t, worker.providers.appMgr.Instances().Remove(remoteRun.ID))
	response = makeAuthRequest(t, coordinator, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, request)
	require.Equal(t, 409, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), "operation_target_unavailable")
	require.Zero(t, localCalls.Load())
	require.EqualValues(t, 1, workerCalls.Load())
}

func TestIntegrationConnectionDiscoveryVisibilityAndNoSideEffects(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	run := seedOperationRun(t, s, "llamacpp", "allowed", upstream.URL)
	_, err := s.services.Teams.CreateTeam(t.Context(), &CreateTeamRequest{ID: "limited", Name: "Limited", AllowedModels: []string{"allowed"}, ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"llamacpp"}}, SpendLimit: 0.01, TPMLimit: 1}, "admin")
	require.NoError(t, err)
	key := loadTestKey(t, s, "connection-reader", keys.VirtualKey{Name: "Reader", TeamID: "limited"})
	before := run.ToInfo(s.node.Name())
	for _, scope := range []struct{ path, key string }{{"/zzrouter/v1/connections", TestAdminKey}, {"/inference/v1/connections", TestUserKey}, {"/inference/v1/connections", key}} {
		response := makeAuthRequest(t, s, http.MethodGet, scope.path, scope.key, nil)
		require.Equal(t, 200, response.Code, string(response.Body))
		require.Contains(t, string(response.Body), upstream.URL)
		require.Contains(t, string(response.Body), `"scope":"loopback"`)
		require.Contains(t, string(response.Body), `"remote_reachability":"unknown"`)
		require.NotContains(t, string(response.Body), "advertised.invalid")
	}
	after := run.ToInfo(s.node.Name())
	assert.Equal(t, before.LastActivity, after.LastActivity)
	assert.Equal(t, before.Pinned, after.Pinned)
	assert.Zero(t, calls.Load())
	require.NoError(t, s.model.Groups.Set("assigned", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "allowed", App: "llamacpp", Model: "allowed", Node: s.node.Name()}}}))
	_, err = s.services.Teams.UpdateTeam(t.Context(), "limited", &UpdateTeamRequest{RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"assigned"}}}, "admin")
	require.NoError(t, err)
	response := makeAuthRequest(t, s, http.MethodGet, "/inference/v1/connections", key, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), `"assigned"`)
	_, err = s.services.Teams.UpdateTeam(t.Context(), "limited", &UpdateTeamRequest{ProviderAccess: teams.ProviderAccess{Mode: "none"}}, "admin")
	require.NoError(t, err)
	response = makeAuthRequest(t, s, http.MethodGet, "/inference/v1/connections", key, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.NotContains(t, string(response.Body), upstream.URL)
	run.SetStatus(instance.StatusStopped)
	response = makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/connections", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.NotContains(t, string(response.Body), upstream.URL)
	assert.Equal(t, "https://example.com/api", safeConnectionURL("https://username:password@example.com/api?api_key=private&other=secret#private"))
	assert.Zero(t, calls.Load())
}

func TestIntegrationConnectionDiscoveryProcessWithoutListenerDeclaration(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"mlx"}, enabled: []string{"mlx"}})
	svc, found := s.appsConfig().LookupApp("mlx")
	require.True(t, found)
	require.Nil(t, svc.Runtime.Listener, "regression requires a provider without listener metadata")
	run := seedOperationRun(t, s, "mlx", "model", upstream.URL)
	run.SetResolved(instance.Resolved{Runtime: "mlx", Listener: &instance.ListenerInfo{Scope: "unknown", Authentication: "unknown", Source: "launch_configuration"}})
	before := run.ToInfo(s.node.Name())
	read := func() connectionInfo {
		t.Helper()
		response := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/connections?provider=mlx", TestAdminKey, nil)
		require.Equal(t, 200, response.Code, string(response.Body))
		var result struct {
			Data []connectionInfo `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body, &result))
		require.Len(t, result.Data, 1)
		return result.Data[0]
	}
	view := read()
	require.Equal(t, strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1), view.NativeURL, "the canonical local upstream supplies the address, never advertised BaseURL")
	require.Equal(t, "unknown", view.Listener.Scope)
	require.Equal(t, "unknown", view.NativeAuthentication)
	require.Equal(t, "unknown", view.RemoteReachability)
	require.Equal(t, "run_upstream", view.Listener.Source)
	require.NotContains(t, view.NativeURL, "advertised.invalid")
	after := run.ToInfo(s.node.Name())
	require.Equal(t, before.LastActivity, after.LastActivity)
	require.Equal(t, before.ActiveRequests, after.ActiveRequests)
	require.Equal(t, before.Pinned, after.Pinned)
	run.SetStatus(instance.StatusStopped)
	view = read()
	require.Empty(t, view.NativeURL)
	require.Empty(t, view.Listener.URL)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/zzrouter/v1/connections", nil)
	view = s.runConnectionInfo(c, instance.InstanceInfo{ID: run.ID, Provider: "mlx", Status: instance.StatusRunning, RecordOnly: true, Port: run.Port, Listener: &instance.ListenerInfo{URL: upstream.URL, Scope: "loopback"}}, connectionEvidence{Source: "fixture"})
	require.Empty(t, view.NativeURL)
	require.Empty(t, view.Listener.URL)
	require.Zero(t, calls.Load(), "native address discovery must not probe or contact the engine")
}

func TestIntegrationProviderDiscoveryPagination(t *testing.T) {
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	for _, path := range []string{"/zzrouter/v1/operations", "/zzrouter/v1/connections"} {
		for _, query := range []string{"?offset=-1", "?limit=0", "?limit=101", "?offset=9223372036854775808", "?limit=invalid"} {
			response := makeAuthRequest(t, s, http.MethodGet, path+query, TestAdminKey, nil)
			require.Equal(t, 400, response.Code, string(response.Body))
		}
		response := makeAuthRequest(t, s, http.MethodGet, path+"?offset=9223372036854775807&limit=100", TestAdminKey, nil)
		require.Equal(t, 200, response.Code, string(response.Body))
		require.Contains(t, string(response.Body), `"data":[]`)
		require.Contains(t, string(response.Body), `"has_more":false`)
	}
	response := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/operations?limit=1", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), `"has_more":true`)
}

func TestIntegrationOperationRequiresInputAndCopiedTargetFields(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"tokens":[1]}`)) }))
	defer upstream.Close()
	s := operationTestNode(t, "coordinator", config.ClusterModeDisabled)
	run := seedOperationRun(t, s, "llamacpp", "model", upstream.URL)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		operation := svc.Operations["tokenize"]
		operation.InputSchema = config.OperationSchema{"type": []string{"object", "null"}}
		svc.Operations["tokenize"] = operation
		return nil
	}))
	request := operationTestRequest(t, s, run)
	request.Input = nil
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	for _, field := range []string{"input", "model", "runtime", "null model", "null runtime"} {
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(encoded, &body))
		if field == "input" {
			delete(body, "input")
		} else {
			var target map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body["target"], &target))
			if strings.HasPrefix(field, "null ") {
				target[strings.TrimPrefix(field, "null ")] = json.RawMessage(`null`)
			} else {
				delete(target, field)
			}
			body["target"], err = json.Marshal(target)
			require.NoError(t, err)
		}
		response := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, body)
		require.Equal(t, 400, response.Code, string(response.Body))
	}
	response := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/operations/llamacpp.tokenize/execute", TestAdminKey, request)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.EqualValues(t, 1, calls.Load())
}

func TestIntegrationWorkerDaemonConnectionsWithoutLoadedModel(t *testing.T) {
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer daemon.Close()
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: config.ClusterModeWorker}, providers: []string{"ollama", "openai"}, enabled: []string{"ollama", "openai"}})
	require.NoError(t, worker.configStore.ApplyParameterPatch("ollama", func(svc *config.ServiceConfig) error {
		svc.Runtime.Endpoint = daemon.URL + "/native?api_key=fixture-private#private"
		return nil
	}))
	coordinator := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	linkOperationWorker(t, coordinator, worker)
	read := func() []connectionInfo {
		t.Helper()
		response := makeAuthRequest(t, coordinator, http.MethodGet, "/zzrouter/v1/connections?node=worker", TestAdminKey, nil)
		require.Equal(t, 200, response.Code, string(response.Body))
		require.NotContains(t, string(response.Body), "fixture-private")
		require.NotContains(t, string(response.Body), `"provider":"openai"`, "hosted connections remain coordinator-owned")
		var result struct {
			Data []connectionInfo `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body, &result))
		return result.Data
	}
	rows := read()
	require.Len(t, rows, 1)
	require.Equal(t, "ollama", rows[0].Provider)
	require.Equal(t, "worker", rows[0].Node)
	require.Equal(t, daemon.URL+"/native", rows[0].NativeURL)
	require.Equal(t, "configured", rows[0].State)
	require.Empty(t, rows[0].RunID)
	run := instance.NewInstance("served-ollama", "ollama", "model", 0, time.Minute, 1)
	run.SetResolved(instance.Resolved{Runtime: "ollama"})
	run.SetStatus(instance.StatusRunning)
	require.NoError(t, worker.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { run.Cancel(); _ = worker.providers.appMgr.Instances().Remove(run.ID) })
	rows = read()
	require.Len(t, rows, 2)
	found := false
	for _, row := range rows {
		if row.RunID == run.ID {
			found = true
			require.Equal(t, daemon.URL+"/native", row.NativeURL)
			require.NotEmpty(t, row.ConnectionRevision)
		}
	}
	require.True(t, found)
	run.SetStatus(instance.StatusStopped)
	for _, row := range read() {
		if row.RunID == run.ID {
			require.Empty(t, row.NativeURL)
		}
	}
	require.Zero(t, calls.Load(), "metadata must not probe a daemon")
}

func linkOperationWorker(t *testing.T, coordinator, worker *Server) {
	t.Helper()
	owner := transportNode(t, clusternode.Coordinator, "coordinator", nil)
	workerNode := transportNode(t, clusternode.Unclaimed, "worker", func(*clusternode.Node) http.Handler { return buildInternalEngine(worker) })
	workerURL := enrollTransportNode(t, owner, workerNode)
	client, err := owner.DialClient(clusterid.RoleWorker.OU())
	require.NoError(t, err)
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coordinator"}, "http://coordinator.invalid", coordinator, nil, mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: client, ClusterPort: workerNode.Addr().(*net.TCPAddr).Port}))
	require.NoError(t, err)
	t.Cleanup(cluster.Stop)
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerURL, ClusterURL: workerURL, NodeName: "worker", Status: mesh.StatusUp}))
	coordinator.cluster.coordinator.Stop()
	coordinator.cluster.coordinator = cluster
	coordinator.cluster.router = routing.NewClusterAwareRouterWithClient("coordinator", cluster.GetClient(), "0", coordinator.ServeLocalRequest, client)
}
