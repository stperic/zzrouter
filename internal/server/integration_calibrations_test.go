package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/calibration"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	calibrationPath        = "/zzrouter/v1/calibrations"
	calibrationWorkerAlias = "calibration-worker-alias"
)

func calibrationFixtureRun(t *testing.T, s *Server, id, backend string, status instance.Status, endpoint string, api ...string) *instance.Instance {
	t.Helper()
	u, err := url.Parse(backend)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	run := instance.NewInstance(id, "llamacpp", "demo", port, time.Minute, 0)
	run.Endpoint = endpoint
	run.Config = instance.Config{Provider: "llamacpp", Model: "demo", Endpoint: endpoint, LaunchMode: instance.LaunchModeNative}
	run.InitConcurrencyLimit(instance.ConcurrencyConfig{MaxConcurrentRequests: 2})
	run.SetResolved(instance.Resolved{Runtime: "llamacpp", WireEndpoints: api, Parameters: map[string]string{"ctx-size": "4096"},
		Listener: &instance.ListenerInfo{URL: backend}})
	run.SetLoaded(instance.Loaded{ContextLength: 4096, Cache: instance.CacheCapacity{KVTokens: 8192, ContextTokens: 4096, MaxConcurrency: 2}})
	if status == instance.StatusRunning {
		require.True(t, run.MarkRunning(false, nil))
	} else {
		run.SetStatus(status)
	}
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { run.Cancel(); _ = s.providers.appMgr.Instances().Remove(run.ID) })
	return run
}

func calibrationRequestFor(node, id string, steps ...int) calibration.Request {
	return calibration.Request{Node: node, Runs: []calibration.RunRequest{{RunID: id, Workload: &calibration.Workload{
		InputTokens: 40, OutputTokens: new(2), Concurrency: steps, RequestsPerStep: new(2),
	}}}}
}

func calibrationHTTP(t *testing.T, handler http.Handler, method, path, key string, input any) *httptest.ResponseRecorder {
	t.Helper()
	var content []byte
	if input != nil {
		var err error
		content, err = json.Marshal(input)
		require.NoError(t, err)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(content))
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func calibrationJobID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("Location"))
	var result struct{ Data calibrationAccepted }
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.NotEmpty(t, result.Data.Node)
	require.Equal(t, jobs.KindCalibration, jobs.KindFromID(result.Data.JobID))
	return result.Data.JobID
}

func awaitCalibration(t *testing.T, source *jobs.Registry, id string, want jobs.Phase) jobs.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	sub, err := source.Subscribe(id, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("calibration job did not finish")
		case event, ok := <-sub.Events():
			require.True(t, ok)
			if event.Phase.IsTerminal() {
				require.Equal(t, want, event.Phase, event.Err)
				return event
			}
		}
	}
}

func calibrationReportOf(t *testing.T, event jobs.Event) calibration.Report {
	t.Helper()
	data, err := json.Marshal(event.Meta["report"])
	require.NoError(t, err)
	require.NotEqual(t, "null", string(data))
	var report calibration.Report
	require.NoError(t, json.Unmarshal(data, &report))
	return report
}

func TestIntegrationCalibrationValidation(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	run := calibrationFixtureRun(t, s, "run_checked", "http://127.0.0.1:1", instance.StatusRunning, calibration.Chat, "chat_completions")
	for _, test := range []struct {
		code, tool string
		status     int
		alter      func(*calibration.Request)
	}{
		{"invalid_value", "zz_calibrate", 400, func(r *calibration.Request) { r.Runs[0].Workload.InputTokens = 0; r.Runs[0].RunID = "absent" }},
		{"run_not_found", "zz_runs", 404, func(r *calibration.Request) { r.Runs[0].RunID = "absent" }},
		{"run_not_ready", "zz_runs", 409, func(r *calibration.Request) {
			run.SetStatus(instance.StatusStarting)
			r.Runs[0].TargetRevision = new("stale")
		}},
		{"run_changed", "zz_runs", 409, func(r *calibration.Request) { r.Runs[0].TargetRevision = new("stale") }},
		{"calibration_unsupported", "zz_runs", 409, func(r *calibration.Request) {
			run.SetResolved(instance.Resolved{WireEndpoints: []string{"embeddings"}})
		}},
		{"context_too_small", "zz_params_set", 400, func(r *calibration.Request) {
			r.Runs[0].Workload.InputTokens = 4095
			r.Runs[0].Workload.Concurrency = []int{3}
			r.Runs[0].Workload.RequestsPerStep = new(3)
		}},
		{"concurrency_above_limit", "zz_params_set", 400, func(r *calibration.Request) {
			r.Runs[0].Workload.Concurrency = []int{3}
			r.Runs[0].Workload.RequestsPerStep = new(3)
		}},
	} {
		t.Run(test.code, func(t *testing.T) {
			initial := run.Resolved()
			defer run.SetResolved(initial)
			defer run.SetStatus(instance.StatusRunning)
			req := calibrationRequestFor("local", run.ID, 1)
			test.alter(&req)
			response := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", req)
			require.Equal(t, test.status, response.Code, response.Body.String())
			require.Contains(t, response.Header().Get("Content-Type"), "application/problem+json")
			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			require.Equal(t, test.code, problem.Code)
			require.Contains(t, problem.Detail, test.tool)
			if test.code == "invalid_value" {
				require.Len(t, problem.Errors, 1)
				require.Equal(t, "runs[0].workload.input_tokens", problem.Errors[0].Key)
				require.Empty(t, problem.RunID)
			} else {
				require.Equal(t, req.Runs[0].RunID, problem.RunID)
			}
			require.Empty(t, problem.JobID)
			require.Empty(t, s.jobs.List(jobs.KindCalibration))
		})
	}
}

func TestCalibrationInternalRefusesWrongNode(t *testing.T) {
	s := operationTestNode(t, "receiver", config.ClusterModeWorker)
	run := calibrationFixtureRun(t, s, "run_present", "http://127.0.0.1:1", instance.StatusRunning, calibration.Chat, "chat_completions")
	response := calibrationHTTP(t, buildInternalEngine(s), http.MethodPost, internalCalibrationsPath, "", calibrationRequestFor("intended", run.ID, 1))
	require.Equal(t, http.StatusMisdirectedRequest, response.Code, response.Body.String())
	require.Contains(t, response.Header().Get("Content-Type"), "application/problem+json")
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	require.Equal(t, "calibration_wrong_node", problem.Code)
	require.Equal(t, "receiver", problem.Node)
	require.Contains(t, problem.Detail, `"receiver"`)
	require.Empty(t, s.jobs.List(jobs.KindCalibration))
}

func TestCalibrationCanonicalizesCoordinatorAliases(t *testing.T) {
	s := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	calibrationDefaults(t, s)
	engine := calibrationChatHandler(t, nil)
	client := &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec.Result(), nil
	})}
	s.httpClient, s.httpStreamingClient = client, client
	run := calibrationFixtureRun(t, s, "run_alias", "http://127.0.0.1:1", instance.StatusRunning, calibration.Chat, "chat_completions")
	address := net.JoinHostPort(s.node.Bind(), strconv.Itoa(s.node.Port()))
	for _, selector := range []string{"@master", address, "http://" + address} {
		t.Run(selector, func(t *testing.T) {
			response := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", calibrationRequestFor(selector, run.ID, 1))
			id := calibrationJobID(t, response)
			require.Contains(t, response.Body.String(), `"node":"coordinator"`)
			report := calibrationReportOf(t, awaitCalibration(t, s.jobs, id, jobs.PhaseDone))
			require.Equal(t, "coordinator", report.Request.Node)
		})
	}
}

func linkCalibrationWorker(t *testing.T, coordinator, worker *Server) (http.Handler, *clusternode.Node, *atomic.Int32) {
	t.Helper()
	var internalCalls atomic.Int32
	owner := transportNode(t, clusternode.Coordinator, "coordinator", nil)
	remote := transportNode(t, clusternode.Unclaimed, "worker", func(*clusternode.Node) http.Handler {
		internal := buildInternalEngine(worker)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == internalCalibrationsPath {
				internalCalls.Add(1)
				assert.Empty(t, r.Header.Get("Idempotency-Key"))
			}
			internal.ServeHTTP(w, r)
		})
	})
	workerURL := enrollTransportNode(t, owner, remote)
	client, err := owner.DialClient(clusterid.RoleWorker.OU())
	require.NoError(t, err)
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coordinator"}, "http://coordinator.invalid", coordinator, nil,
		mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: client, ClusterPort: remote.Addr().(*net.TCPAddr).Port}))
	require.NoError(t, err)
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerURL, ClusterURL: workerURL, Alias: calibrationWorkerAlias, NodeName: "worker", Status: mesh.StatusUp}))
	useTestCluster(t, coordinator, cluster)
	coordinator.cluster.listener = owner
	coordinator.cluster.router = routing.NewClusterAwareRouterWithClient("coordinator", cluster.GetClient(), "0", coordinator.ServeLocalRequest, client)
	coordinator.services.Runs.router = coordinator.cluster.router
	require.NotNil(t, coordinator.jobsProxy)
	return coordinator.engine, remote, &internalCalls
}

func calibrationBackend(t *testing.T, before func(*http.Request, int) bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(calibrationChatHandler(t, before))
	t.Cleanup(server.Close)
	return server
}

func calibrationChatHandler(t *testing.T, before func(*http.Request, int) bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = io.WriteString(w, "llamacpp:requests_processing 1\nllamacpp:requests_deferred 0\n")
			return
		}
		var body struct {
			Stream        bool
			Model         string
			Messages      []struct{ Content string }
			MaxTokens     int  `json:"max_tokens"`
			IgnoreEOS     bool `json:"ignore_eos"`
			Seed          *int
			Temperature   *float64
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) || !assert.NotEmpty(t, body.Messages) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, chatCompletionsPath, r.URL.Path)
		assert.Equal(t, "demo", body.Model)
		assert.True(t, body.Stream)
		assert.True(t, body.IgnoreEOS)
		assert.True(t, body.StreamOptions.IncludeUsage)
		assert.Nil(t, body.Seed)
		assert.Equal(t, new(0.4), body.Temperature)
		assert.Empty(t, r.Header.Get("X-API-Key"))
		if before != nil && !before(r, body.MaxTokens) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: {\"object\":\"chat.completion\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"cost\":0.1}}\n\ndata: [DONE]\n\n", 2+len(body.Messages[0].Content)/8, body.MaxTokens)
	})
}

func calibrationDefaults(t *testing.T, s *Server) {
	t.Helper()
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(cfg *config.ServiceConfig) error {
		if cfg.Models == nil {
			cfg.Models = map[string]config.ModelSpec{}
		}
		cfg.Models["demo"] = config.ModelSpec{Request: map[string]any{"temperature": 0.4, "Seed": 71}}
		return nil
	}))
}

func TestIntegrationCalibrationWorkerJobReplayAndReport(t *testing.T) {
	worker := operationTestNode(t, "worker", config.ClusterModeWorker)
	coordinator := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	calibrationDefaults(t, worker)
	handler, _, forwarded := linkCalibrationWorker(t, coordinator, worker)
	started, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	backend := calibrationBackend(t, func(r *http.Request, _ int) bool {
		if first.CompareAndSwap(false, true) {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return false
			}
		}
		return true
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	run := calibrationFixtureRun(t, worker, "run_worker", backend.URL, instance.StatusRunning, calibration.Chat, "chat_completions")
	req := calibrationRequestFor("http://"+calibrationWorkerAlias+":9090", run.ID, 1)
	req.Runs[0].TargetRevision = new(run.ToInfo("worker").TargetRevision)
	accepted := calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "calibration-replay", req)
	id := calibrationJobID(t, accepted)
	require.Contains(t, accepted.Body.String(), `"node":"worker"`)
	awaitDownloadSignal(t, started)
	t.Run("idempotent_replay", func(t *testing.T) {
		replayed := calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "calibration-replay", req)
		require.Equal(t, id, calibrationJobID(t, replayed))
		require.EqualValues(t, 1, forwarded.Load())
	})
	t.Run("retried_forward_reuses_job", func(t *testing.T) {
		canonical := req
		canonical.Node = "worker"
		replayed := calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "", canonical)
		require.Equal(t, id, calibrationJobID(t, replayed))
		require.EqualValues(t, 2, forwarded.Load())
	})
	t.Run("calibration_running", func(t *testing.T) {
		other := calibrationRequestFor("worker", run.ID, 2)
		busy := calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "", other)
		require.Equal(t, 409, busy.Code, busy.Body.String())
		var problem utils.ProblemDetails
		require.NoError(t, json.Unmarshal(busy.Body.Bytes(), &problem))
		require.Equal(t, "calibration_running", problem.Code)
		require.Equal(t, id, problem.JobID)
		require.Empty(t, problem.RunID)
		require.Contains(t, problem.Detail, "zz_job")
		require.Len(t, worker.jobs.List(jobs.KindCalibration), 1)
	})
	close(release)
	event := awaitCalibration(t, worker.jobs, id, jobs.PhaseDone)
	report := calibrationReportOf(t, event)
	require.Equal(t, "worker", report.Request.Node)
	require.EqualValues(t, "verified", report.Outcome)
	require.Len(t, report.Steps, 1)
	require.Equal(t, new(uint64(0)), report.Steps[0].Runs[0].PublicRequests)
	require.Equal(t, new(1), report.Steps[0].Runs[0].Engine.Running)
	runReport := report.Runs[0]
	require.Equal(t, run.ID, runReport.RunID)
	require.Equal(t, "4096", runReport.ResolvedParameters["ctx-size"])
	require.Equal(t, new(4096), runReport.ServedContextTokens)
	require.Equal(t, new(2), runReport.MaxConcurrentRequests)
	require.Equal(t, new(int64(8192)), runReport.Cache.KVTokens)
	require.True(t, runReport.OutputForced)
	require.NotEmpty(t, runReport.ParametersStatus)
	require.Equal(t, new("chat-v1"), runReport.WorkloadRevision)
	require.Equal(t, req.Runs[0].TargetRevision, report.Request.Runs[0].TargetRevision)
	jobPath := "/zzrouter/v1/jobs/" + id
	read := calibrationHTTP(t, handler, http.MethodGet, jobPath+"?node=worker", "", nil)
	require.Equal(t, 200, read.Code, read.Body.String())
	require.Contains(t, read.Body.String(), `"report"`)
	stream := calibrationHTTP(t, handler, http.MethodGet, jobPath+"/stream?node=worker", "", nil)
	require.Equal(t, 200, stream.Code, stream.Body.String())
	require.Contains(t, stream.Body.String(), "event:done")
	require.Equal(t, 1, strings.Count(stream.Body.String(), `"report"`))

	runRead := calibrationHTTP(t, handler, http.MethodGet, "/zzrouter/v1/runs/"+run.ID+"?node=worker", "", nil)
	require.Equal(t, 200, runRead.Code, runRead.Body.String())
	require.Contains(t, runRead.Body.String(), `"target_revision":"`+*req.Runs[0].TargetRevision+`"`)
	require.Contains(t, runRead.Body.String(), `"requests_admitted":4`)
	discovery := calibrationHTTP(t, handler, http.MethodGet, "/zzrouter/v1", "", nil)
	require.Equal(t, 200, discovery.Code, discovery.Body.String())
	require.Contains(t, discovery.Body.String(), `"max_step_timeout_seconds":1800`)
	require.Nil(t, buildAPIDiscovery(worker).Calibration)
}

func TestIntegrationCalibrationUnreachableWorker(t *testing.T) {
	worker := operationTestNode(t, "worker", config.ClusterModeWorker)
	coordinator := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	handler, remote, _ := linkCalibrationWorker(t, coordinator, worker)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, remote.Stop(ctx))
	result := calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "", calibrationRequestFor("worker", "absent", 1))
	require.Equal(t, 503, result.Code, result.Body.String())
	require.Contains(t, result.Body.String(), "node_unreachable")
}

func TestIntegrationCalibrationRestartMidStep(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	calibrationDefaults(t, s)
	entered := make(chan struct{})
	var once atomic.Bool
	upstream := calibrationBackend(t, func(r *http.Request, output int) bool {
		if output == 2 {
			if once.CompareAndSwap(false, true) {
				close(entered)
			}
			<-r.Context().Done()
			return false
		}
		return true
	})
	run := calibrationFixtureRun(t, s, "run_changed", upstream.URL, instance.StatusRunning, calibration.Chat, "chat_completions")
	req := calibrationRequestFor("local", run.ID, 1)
	id := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", req))
	awaitDownloadSignal(t, entered)
	run.SetStatus(instance.StatusStopped)
	require.NoError(t, s.providers.appMgr.Instances().Remove(run.ID))
	replacement := calibrationFixtureRun(t, s, run.ID, upstream.URL, instance.StatusRunning, calibration.Chat, "chat_completions")
	require.NotEqual(t, run.ToInfo("local").TargetRevision, replacement.ToInfo("local").TargetRevision)
	run.Cancel()
	report := calibrationReportOf(t, awaitCalibration(t, s.jobs, id, jobs.PhaseDone))
	require.EqualValues(t, "inconclusive", report.Outcome)
	require.Equal(t, "run_changed", report.Steps[0].Runs[0].Reason.Code)
	require.Nil(t, report.Steps[0].Runs[0].PublicRequests)
}

func TestIntegrationCalibrationCancellationKeepsMeasuredSteps(t *testing.T) {
	worker := operationTestNode(t, "worker", config.ClusterModeWorker)
	coordinator := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	calibrationDefaults(t, worker)
	handler, _, _ := linkCalibrationWorker(t, coordinator, worker)
	entered, stopped := make(chan struct{}), make(chan struct{})
	var measured atomic.Int32
	upstream := calibrationBackend(t, func(r *http.Request, output int) bool {
		if output == 2 && measured.Add(1) == 3 {
			close(entered)
			<-r.Context().Done()
			close(stopped)
			return false
		}
		return true
	})
	run := calibrationFixtureRun(t, worker, "run_cancelled", upstream.URL, instance.StatusRunning, calibration.Chat, "chat_completions")
	id := calibrationJobID(t, calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "", calibrationRequestFor("worker", run.ID, 1, 1)))
	awaitDownloadSignal(t, entered)
	cancelled := calibrationHTTP(t, handler, http.MethodDelete, "/zzrouter/v1/jobs/"+id+"?node=worker", "", nil)
	require.Equal(t, 202, cancelled.Code, cancelled.Body.String())
	event := awaitCalibration(t, worker.jobs, id, jobs.PhaseFailed)
	require.Equal(t, jobs.ErrCanceled.Error(), event.Err)
	report := calibrationReportOf(t, event)
	require.EqualValues(t, "cancelled", report.Outcome)
	require.Len(t, report.Steps, 1)
	require.EqualValues(t, "verified", report.Steps[0].Outcome)
	require.Equal(t, 2, report.Steps[0].Runs[0].Requests.Completed)
	awaitDownloadSignal(t, stopped)
	require.Zero(t, run.GetActiveRequests())
	require.True(t, run.IsHealthy())
	fresh := calibrationJobID(t, calibrationHTTP(t, handler, http.MethodPost, calibrationPath, "", calibrationRequestFor("worker", run.ID, 1)))
	require.NotEqual(t, id, fresh)
	awaitCalibration(t, worker.jobs, fresh, jobs.PhaseDone)
}

func TestIntegrationCalibrationListKeepsThirtyReportsBelowProxyLimit(t *testing.T) {
	worker := operationTestNode(t, "worker", config.ClusterModeWorker)
	coordinator := operationTestNode(t, "coordinator", config.ClusterModeCoordinator)
	handler, _, _ := linkCalibrationWorker(t, coordinator, worker)
	report := calibration.Report{Runs: []calibration.RunReport{{ResolvedParameters: map[string]string{"fixture": strings.Repeat("x", 40<<10)}}}}
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.Greater(t, 30*len(raw), upstreamBodyLimit)
	var last string
	for range 30 {
		job, err := worker.jobs.StartDetached(jobs.KindCalibration, "", nil)
		require.NoError(t, err)
		job.Meta(jobs.Meta{"report": report, "steps": 8})
		job.Done()
		last = job.ID()
	}
	list := calibrationHTTP(t, handler, http.MethodGet, "/zzrouter/v1/jobs?node=worker&kind=calibration&limit=30", "", nil)
	require.Equal(t, 200, list.Code, list.Body.String())
	require.Less(t, list.Body.Len(), upstreamBodyLimit)
	require.Contains(t, list.Body.String(), `"total":30`)
	require.Contains(t, list.Body.String(), `"steps":8`)
	require.NotContains(t, list.Body.String(), `"report"`)
	item := calibrationHTTP(t, handler, http.MethodGet, "/zzrouter/v1/jobs/"+last+"?node=worker", "", nil)
	require.Equal(t, 200, item.Code, item.Body.String())
	require.Contains(t, item.Body.String(), `"report"`)
}

func TestIntegrationCalibrationTrafficDoesNotPublishInference(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	calibrationDefaults(t, s)
	verifyTelemetry := calibrationTelemetry(t, s)

	upstream := calibrationBackend(t, nil)
	run := calibrationFixtureRun(t, s, "run_quiet", upstream.URL, instance.StatusRunning, calibration.Chat, "chat_completions")
	id := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", calibrationRequestFor("local", run.ID, 1)))
	report := calibrationReportOf(t, awaitCalibration(t, s.jobs, id, jobs.PhaseDone))
	require.EqualValues(t, "verified", report.Outcome)
	require.Equal(t, new(uint64(0)), report.Steps[0].Runs[0].PublicRequests)
	verifyTelemetry()
}

func calibrationTelemetry(t *testing.T, s *Server) func() {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	oldMeter := otel.GetMeterProvider()
	otel.SetMeterProvider(meter)
	t.Cleanup(func() { otel.SetMeterProvider(oldMeter); _ = meter.Shutdown(t.Context()) })
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	oldTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(tracer)
	t.Cleanup(func() { otel.SetTracerProvider(oldTracer); _ = tracer.Shutdown(t.Context()) })
	store := inferencelog.NewStore(10, 0)
	bridge := NewInferenceLogBridge(store, "local", false, nil)
	bridge.SetAccessControl(s.access)
	llm.SetInferenceLogHook(bridge)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	beforeSpend := s.access.GetAllSpendStates()
	return func() {
		t.Helper()
		require.Equal(t, beforeSpend, s.access.GetAllSpendStates())
		require.Empty(t, store.Query(inferencelog.QueryFilter{}))
		for _, span := range spans.Ended() {
			for _, attr := range span.Attributes() {
				require.False(t, strings.HasPrefix(string(attr.Key), "gen_ai."))
			}
		}
		var metrics metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(t.Context(), &metrics))
		for _, scope := range metrics.ScopeMetrics {
			for _, data := range scope.Metrics {
				require.False(t, strings.HasPrefix(data.Name, "gen_ai."), data.Name)
			}
		}
	}
}

func TestCalibrationSenderUsesProductionDeadlines(t *testing.T) {
	for _, firstByte := range []bool{true, false} {
		name := "idle"
		code := "upstream_stream_interrupted"
		if firstByte {
			name, code = "first_byte", "upstream_timeout"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, run := instanceStreamTestServer()
				t.Cleanup(run.Cancel)
				require.True(t, run.MarkRunning(false, nil))
				require.NoError(t, s.providers.appMgr.Instances().Register(run))
				revision := run.ToInfo("test-node").TargetRevision
				runner := &nodeCalibration{server: s, node: "test-node", targets: map[string]calibration.Target{
					run.ID: {TargetRevision: revision, Run: calibration.RunReport{Endpoint: calibration.Chat}},
				}}
				s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(upstream *http.Request) (*http.Response, error) {
					if firstByte {
						<-upstream.Context().Done()
						return nil, upstream.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Body: deadlineBody{ctx: upstream.Context(), Reader: strings.NewReader(partialChatStream)}}, nil
				})}
				ctx, cancel := context.WithTimeout(t.Context(), 2*constants.DefaultFirstByteTimeout)
				defer cancel()
				got := runner.Send(ctx, clock.System(), run.ID, revision, map[string]any{"stream": true})
				require.Equal(t, code, got.ErrorCode)
				require.Equal(t, revision, got.TargetRevision)
				require.NotEmpty(t, got.LeaseGranted)
				require.Zero(t, run.GetActiveRequests())
				require.NoError(t, ctx.Err())
			})
		})
	}
}

func TestCalibrationSamplerAttributesOnlyExactDaemonModelSets(t *testing.T) {
	a := instance.NewInstance("a", "ollama", "a:latest", 0, 0, 0)
	b := instance.NewInstance("b", "ollama", "b:latest", 0, 0, 0)
	require.Equal(t, []string{"a", "b"}, daemonRunOwners([]*instance.Instance{a, b}, []string{"b:latest", "a:latest"}))
	require.Empty(t, daemonRunOwners([]*instance.Instance{a}, []string{"a:latest", "b:latest"}))
	require.Empty(t, daemonRunOwners([]*instance.Instance{a}, []string{"b:latest"}))
	require.Empty(t, daemonRunOwners([]*instance.Instance{a}, nil))
	hardware := resources.Snapshot{
		Pools: []resources.Pool{
			{ID: "GPU-0", Kind: resources.GPU, Holders: []resources.Holder{{PID: 1, RunID: "daemon:ollama", MemoryMiB: new(int64(100))}}},
			{ID: "host", Kind: resources.Host},
		},
		Runs: map[string]resources.RunSample{"daemon:ollama": {HostMemoryMiB: new(int64(20))}},
	}
	runs := map[string]calibration.RunSample{"a": {}, "b": {}}
	exact := calibrationPools(hardware, runs, map[string][]string{"daemon:ollama": {"a", "b"}})
	require.Len(t, exact[0].Holders, 1)
	require.Equal(t, []string{"a", "b"}, exact[0].Holders[0].ListedRuns)
	require.Equal(t, new(int64(20)), exact[1].HostMemoryMiB["a"])
	require.Equal(t, new(int64(20)), exact[1].HostMemoryMiB["b"])
	extra := calibrationPools(hardware, runs, map[string][]string{"daemon:ollama": nil})
	require.Nil(t, extra[0].Holders[0].RunID)
	require.Empty(t, extra[0].Holders[0].ListedRuns)
	require.Empty(t, extra[1].HostMemoryMiB)
}

func TestCalibrationLocalJobAndSyntheticSender(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	calibrationDefaults(t, s)
	schema, err := s.configStore.ReadProviderSchemaBytes("llamacpp")
	require.NoError(t, err)
	require.Contains(t, string(schema), "output: {ignore_eos: true}")
	seeded := bytes.Replace(schema, []byte("output: {ignore_eos: true}"), []byte("output: {ignore_eos: true, seed: 72}"), 1)
	require.NoError(t, os.WriteFile(filepath.Join(s.configStore.DirPath(), "on-demand", "llamacpp", "schema.yaml"), seeded, 0o600))
	verifyTelemetry := calibrationTelemetry(t, s)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	engine := calibrationChatHandler(t, func(req *http.Request, _ int) bool {
		if once.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-resume:
			case <-req.Context().Done():
				return false
			}
		}
		return true
	})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	transport := deadlineTransport(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec.Result(), nil
	})
	s.httpClient = &http.Client{Transport: transport}
	s.httpStreamingClient = &http.Client{Transport: transport}
	run := calibrationFixtureRun(t, s, "run_local", "http://127.0.0.1:1", instance.StatusRunning, calibration.Chat, "chat_completions")
	request := calibrationRequestFor("local", run.ID, 1)
	id := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "first", request))
	awaitDownloadSignal(t, entered)
	require.Equal(t, id, calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "first", request)))
	require.Equal(t, id, calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", request)))
	different := calibrationRequestFor("local", run.ID, 2)
	conflict := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "first", different)
	require.Equal(t, http.StatusUnprocessableEntity, conflict.Code, conflict.Body.String())
	require.Contains(t, conflict.Body.String(), `"code":"idempotency_key_conflict"`)
	busy := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", different)
	require.Equal(t, http.StatusConflict, busy.Code, busy.Body.String())
	require.Contains(t, busy.Body.String(), id)
	require.Contains(t, busy.Body.String(), `"code":"calibration_running"`)
	close(resume)

	event := awaitCalibration(t, s.jobs, id, jobs.PhaseDone)
	report := calibrationReportOf(t, event)
	require.EqualValues(t, "verified", report.Outcome)
	require.Len(t, report.Steps, 1)
	result := report.Steps[0].Runs[0]
	require.Equal(t, new(uint64(0)), result.PublicRequests)
	require.Equal(t, new(1), result.Engine.Running)
	require.Equal(t, new(0), result.Engine.Waiting)
	require.Equal(t, &calibration.TokenRange{Min: 40, Max: 40}, result.InputTokens)
	require.NotNil(t, result.TimeToFirstTokenMS)
	require.EqualValues(t, 4, run.ToInfo("local").RequestsAdmitted)
	require.Equal(t, 0, event.Meta["step"])
	require.Equal(t, 1, event.Meta["steps"])
	require.Equal(t, map[string]int{run.ID: 2}, event.Meta["requests_completed"])
	require.Equal(t, map[string]*int{run.ID: new(2)}, event.Meta["requests_total"])
	encoded, err := json.Marshal(event.Meta["sample"])
	require.NoError(t, err)
	var sample map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &sample))
	require.Len(t, sample, 2)
	var at time.Time
	require.NoError(t, json.Unmarshal(sample["at"], &at))
	require.False(t, at.Before(report.StartedAt))
	require.False(t, at.After(report.CompletedAt))
	var pools []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(sample["pools"], &pools))
	require.NotEmpty(t, pools)
	for _, pool := range pools {
		keys := []string{"id", "kind", "vendor", "total_mib", "used_mib", "free_mib", "available_mib", "swap_used_mib", "utilization_pct", "power_w", "cpu_pct", "ownership", "holders"}
		require.Len(t, pool, len(keys))
		for _, key := range keys {
			require.Contains(t, pool, key)
		}
	}
	require.Equal(t, "null", string(pools[0]["ownership"]))
	require.Equal(t, "null", string(pools[0]["holders"]))
	require.Empty(t, s.jobs.List(jobs.KindCalibration)[0].Meta["report"])
	verifyTelemetry()
}

func TestCalibrationCancellationReleasesClaim(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	calibrationDefaults(t, s)
	entered := make(chan struct{})
	interrupted, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	var blocked atomic.Bool
	engine := calibrationChatHandler(t, func(req *http.Request, _ int) bool {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-req.Context().Done()
			close(interrupted)
			<-release
			return false
		}
		return true
	})
	s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec.Result(), nil
	})}
	run := calibrationFixtureRun(t, s, "run_cancelled", "http://127.0.0.1:1", instance.StatusRunning, calibration.Chat, "chat_completions")
	id := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", calibrationRequestFor("local", run.ID, 1, 1)))
	awaitDownloadSignal(t, entered)
	cancelled := calibrationHTTP(t, s.engine, http.MethodDelete, "/zzrouter/v1/jobs/"+id, "", nil)
	require.Equal(t, http.StatusAccepted, cancelled.Code, cancelled.Body.String())
	awaitDownloadSignal(t, interrupted)
	request := calibrationRequestFor("local", run.ID, 1)
	for _, entry := range []calibration.Request{request, calibrationRequestFor("local", "absent", 1)} {
		busy := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", entry)
		require.Equal(t, http.StatusConflict, busy.Code, busy.Body.String())
		var problem utils.ProblemDetails
		require.NoError(t, json.Unmarshal(busy.Body.Bytes(), &problem))
		require.Equal(t, "calibration_running", problem.Code)
		require.Equal(t, id, problem.JobID)
	}
	current, err := s.jobs.Get(id)
	require.NoError(t, err)
	require.False(t, current.Phase.IsTerminal())
	finish()
	awaitCalibration(t, s.jobs, id, jobs.PhaseFailed)

	fresh := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", request))
	require.NotEqual(t, id, fresh)
	awaitCalibration(t, s.jobs, fresh, jobs.PhaseDone)
}

func TestCalibrationShutdownFailsWithMeasuredSteps(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	calibrationDefaults(t, s)
	entered := make(chan struct{})
	var measured atomic.Int32
	engine := calibrationChatHandler(t, func(req *http.Request, output int) bool {
		if output == 2 && measured.Add(1) == 3 {
			close(entered)
			<-req.Context().Done()
			return false
		}
		return true
	})
	s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec.Result(), nil
	})}
	run := calibrationFixtureRun(t, s, "run_shutdown", "http://127.0.0.1:1", instance.StatusRunning, calibration.Chat, "chat_completions")
	id := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", calibrationRequestFor("local", run.ID, 1, 1)))
	awaitDownloadSignal(t, entered)
	s.shutdownCancel()
	event := awaitCalibration(t, s.jobs, id, jobs.PhaseFailed)
	require.Equal(t, context.Canceled.Error(), event.Err)
	report := calibrationReportOf(t, event)
	require.EqualValues(t, "cancelled", report.Outcome)
	require.Len(t, report.Steps, 1)
	require.Equal(t, 2, report.Steps[0].Runs[0].Requests.Completed)
}

type calibrationTerminalHandle struct {
	jobs.Handle
	beforeTerminal func()
	panicValue     string
}

func (h calibrationTerminalHandle) Progress(int, string, jobs.Bytes) { panic(h.panicValue) }

func (h calibrationTerminalHandle) Fail(err error) {
	h.beforeTerminal()
	h.Handle.Fail(err)
}

func TestCalibrationRunnerPanicFailsBeforeReleasingClaim(t *testing.T) {
	const panicValue = "private runner panic fixture"
	logs, err := os.CreateTemp(t.TempDir(), "panic-log")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, logs.Close()) })
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	reg := newTestJobsRegistry(t)
	job, err := reg.StartDetached(jobs.KindCalibration, "", nil)
	require.NoError(t, err)
	claim := &calibrationClaim{jobID: job.ID()}
	handle := calibrationTerminalHandle{Handle: job, panicValue: panicValue, beforeTerminal: func() {
		require.Equal(t, job.ID(), claim.jobID)
	}}
	runner := &nodeCalibration{}
	runner.run(handle, calibration.Request{}, claim)
	event := awaitCalibration(t, reg, job.ID(), jobs.PhaseFailed)
	require.Equal(t, calibration.ErrRunnerPanic.Error(), event.Err)
	require.NotContains(t, event.Meta, "report")
	require.Empty(t, claim.jobID)
	logged, err := os.ReadFile(logs.Name())
	require.NoError(t, err)
	require.Contains(t, string(logged), `"level":"ERROR"`)
	require.Contains(t, string(logged), `"panic":"`+panicValue+`"`)
	require.Contains(t, string(logged), `"stack":"goroutine `)
}

func TestCalibrationProgressWithZeroStepsAndUnknownTotal(t *testing.T) {
	registry := newTestJobsRegistry(t)
	job, err := registry.StartDetached(jobs.KindCalibration, "", nil)
	require.NoError(t, err)
	publishCalibrationProgress(job, calibration.Progress{
		Phase: calibration.Checking, Step: 1, Runs: map[string]calibration.RequestCounts{"run_pending": {}},
	}, nil)
	event, err := registry.Get(job.ID())
	require.NoError(t, err)
	require.Zero(t, event.Percent)
	completed, err := json.Marshal(event.Meta["requests_completed"])
	require.NoError(t, err)
	require.JSONEq(t, `{"run_pending":0}`, string(completed))
	total, err := json.Marshal(event.Meta["requests_total"])
	require.NoError(t, err)
	require.JSONEq(t, `{"run_pending":null}`, string(total))
}

func TestCalibrationOpenAPISchemaMatchesJobMetadata(t *testing.T) {
	registry := newTestJobsRegistry(t)
	job, err := registry.StartDetached(jobs.KindCalibration, "", nil)
	require.NoError(t, err)
	job.Progress(0, calibration.Checking, jobs.Bytes{})
	first, err := registry.Get(job.ID())
	require.NoError(t, err)
	body, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"meta"`)
	publishCalibrationProgress(job, calibration.Progress{Phase: calibration.Checking}, nil)
	job.Meta(jobs.Meta{"report": calibration.Report{}})
	job.Done()
	last, err := registry.Get(job.ID())
	require.NoError(t, err)
	require.Len(t, last.Meta, 6)
	schemas, ok := mapAt(loadSpecTree(t)["components"], "schemas")
	require.True(t, ok)
	progress, ok := mapAt(schemas, "CalibrationProgress")
	require.True(t, ok)
	require.Empty(t, progress["required"])
	properties, ok := mapAt(progress, "properties")
	require.True(t, ok)
	for key := range last.Meta {
		require.Contains(t, properties, key)
	}
	steps, ok := mapAt(properties, "steps")
	require.True(t, ok)
	require.Equal(t, last.Meta["steps"], steps["minimum"])
	report, ok := mapAt(properties, "report")
	require.True(t, ok)
	require.Equal(t, "#/components/schemas/CalibrationReport", report["$ref"])
}

func TestCalibrationProgressUsesReportOwnership(t *testing.T) {
	targets := map[string]calibration.Target{"a": {}, "b": {}}
	at := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	measured := []calibration.PoolSample{
		{ID: "dedicated", Kind: "gpu", Vendor: new("nvidia"), TotalMiB: new(int64(50)), Holders: []calibration.Holder{
			{PID: 1, RunID: new("a"), MemoryMiB: new(int64(5))},
			{PID: 2, ListedRuns: []string{"a", "b"}, MemoryMiB: new(int64(10))},
		}},
		{ID: "shared", Kind: "gpu", Holders: []calibration.Holder{{PID: 3, MemoryMiB: new(int64(10))}}},
		{ID: "unknown", Kind: "gpu"},
	}
	meta := calibrationProgress(calibration.Progress{SampleAt: at, Snapshot: calibration.Snapshot{Pools: measured}}, targets)
	observation := meta["sample"].(calibrationLiveSample)
	require.Equal(t, at, observation.At)
	require.Len(t, observation.Pools, 3)
	report := calibration.DescribePools(measured, targets)
	require.Equal(t, report[0], observation.Pools[0].PoolReport)
	require.Equal(t, report[1], observation.Pools[1].PoolReport)
	require.Equal(t, new("verified"), observation.Pools[0].Ownership)
	require.Equal(t, new("shared"), observation.Pools[1].Ownership)
	require.Nil(t, observation.Pools[2].Ownership)
	require.Equal(t, new("a"), observation.Pools[0].Holders[0].RunID)
	require.Nil(t, observation.Pools[0].Holders[1].RunID)
}

func TestCalibrationEmbeddingsSender(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	var hits atomic.Int32
	s.httpStreamingClient = &http.Client{Transport: deadlineTransport(func(req *http.Request) (*http.Response, error) {
		hits.Add(1)
		assert.Equal(t, "/v1/embeddings", req.URL.Path)
		var body struct {
			Model  string
			Input  string
			Stream bool
		}
		assert.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		assert.Equal(t, "demo", body.Model)
		assert.NotEmpty(t, body.Input)
		assert.False(t, body.Stream)
		response := fmt.Sprintf(`{"data":[{"embedding":[1]}],"usage":{"prompt_tokens":%d}}`, len(body.Input)/8)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	run := calibrationFixtureRun(t, s, "run_embed", "http://127.0.0.1:1", instance.StatusRunning, calibration.Embeddings, "embeddings")
	request := calibrationRequestFor("local", run.ID, 1)
	request.Runs[0].Workload.OutputTokens = nil
	id := calibrationJobID(t, calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", request))
	report := calibrationReportOf(t, awaitCalibration(t, s.jobs, id, jobs.PhaseDone))
	require.EqualValues(t, "verified", report.Outcome)
	require.EqualValues(t, 4, hits.Load())
	step := report.Steps[0].Runs[0]
	require.Equal(t, &calibration.TokenRange{Min: 40, Max: 40}, step.InputTokens)
	require.Nil(t, step.OutputTokens)
	require.Nil(t, step.ConcurrentStreamsPeak)
	require.Equal(t, new(uint64(0)), step.PublicRequests)
}

func TestCalibrationReportRedactsCredentials(t *testing.T) {
	envFixture := t.Name() + "-environment-value"
	info := instance.InstanceInfo{ID: "a", ResolvedParameters: map[string]string{
		"ctx-size": "4096", "api-key": "test-key-value", "engine-note": "before sk-123456789012345678901234 after",
		"literal": "[REDACTED]", "structured": `"token":"[REDACTED]"`,
	}, Environment: map[string]string{"SECRET": envFixture}, ParametersStatus: &instance.ParametersStatus{
		State: instance.ParametersCurrent, Overridden: []string{"parameters.api-key", "parameters.ctx_size", "environment.SECRET"},
	}}
	target, _ := calibrationTarget(info, loadProviderSchema(nil, "llamacpp"))
	payload, err := json.Marshal(target.Run)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"ctx-size":"4096"`)
	require.Contains(t, string(payload), `"redacted_parameters":["api-key","engine-note","structured"]`)
	require.Contains(t, string(payload), `"overridden":["api-key","ctx-size","environment.SECRET"]`)
	require.NotContains(t, string(payload), "test-key-value")
	require.NotContains(t, string(payload), "sk-123456789012345678901234")
	require.NotContains(t, string(payload), envFixture)
}

func TestCalibrationRejectsBroadcastSelectors(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	for _, node := range []string{"*", "/*", "/*/*", "*:9090", "http:///"} {
		t.Run(node, func(t *testing.T) {
			response := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", calibrationRequestFor(node, "run_demo", 1))
			require.Equal(t, 400, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"key":"node"`)
		})
	}
}

func TestCalibrationWorkerTransportFailureIs503(t *testing.T) {
	s := operationTestNode(t, "local", config.ClusterModeDisabled)
	s.cluster.router = &unreachableRouter{down: "worker"}
	result := calibrationHTTP(t, s.engine, http.MethodPost, calibrationPath, "", calibrationRequestFor("worker", "run_demo", 1))
	require.Equal(t, 503, result.Code, result.Body.String())
	require.Contains(t, result.Body.String(), "node_unreachable")
	require.Contains(t, result.Body.String(), "zz_nodes")
}
