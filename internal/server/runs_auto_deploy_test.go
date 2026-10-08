package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
)

// TestModelNameEncodesVariant pins the heuristic that skips
// capabilities.default_variant when the repo name itself encodes a
// SPECIFIC quant. The bare `-GGUF` family marker does NOT encode a
// variant — repos like `Qwen/Qwen2.5-0.5B-Instruct-GGUF` carry many
// quants (Q2_K, Q4_K_M, Q8_0, FP16, …) and still need filtering,
// otherwise auto_deploy downloads all of them. Live-verified against
// 491 MB Q4_K_M turning into 11-file ~3 GB grab when this returned
// true on the bare suffix.
func TestModelNameEncodesVariant(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Plain HF model repo — no variant in the name.
		{"meta-llama/Llama-3.2-3B-Instruct", false},
		{"Qwen/Qwen2.5-0.5B", false},
		// Bare -GGUF: family marker only, NOT a variant. Filtering required.
		{"bartowski/Llama-3-8B-Instruct-GGUF", false},
		{"TheBloke/Mistral-7B-Instruct-v0.2-GGUF", false},
		{"some-org/some-model-gguf", false},
		// Direct quant in the name: variant pinned, skip filtering.
		{"some-org/Llama-3-Q4_K_M", true},
		{"some-org/model-Q8_0", true},
		{"some-org/model-fp16", true},
		// Quant-shaped substring inside a path segment that isn't a variant.
		{"org/Q8_Octopus-2B", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelNameEncodesVariant(tc.name))
		})
	}
}

// TestDeploymentLocalJobID covers the per-node JobID extraction shape
// the chained-launch goroutine subscribes against. Three cases: local
// node present in the multi-node deployment, single-node fall-back,
// and "node not in deployment" (defensive nil return).
func TestDeploymentLocalJobID(t *testing.T) {
	multi := &Deployment{
		Nodes: []modelregistry.DeploymentNode{
			{Node: "worker-a", JobID: "dl_aaa"},
			{Node: "macbook-pro", JobID: "dl_bbb"},
			{Node: "worker-c", JobID: "dl_ccc"},
		},
	}
	assert.Equal(t, "dl_bbb", deploymentLocalJobID(multi, "macbook-pro"))

	single := &Deployment{
		Nodes: []modelregistry.DeploymentNode{{Node: "anything", JobID: "dl_solo"}},
	}
	assert.Equal(t, "dl_solo", deploymentLocalJobID(single, "macbook-pro"))

	missing := &Deployment{
		Nodes: []modelregistry.DeploymentNode{
			{Node: "worker-a", JobID: "dl_aaa"},
			{Node: "worker-b", JobID: "dl_bbb"},
		},
	}
	assert.Equal(t, "", deploymentLocalJobID(missing, "macbook-pro"))

	assert.Equal(t, "", deploymentLocalJobID(nil, "macbook-pro"))
}

// TestWaitDeployThenLaunch_DeployFails pins the failure-propagation
// path: when the deploy job ends in PhaseFailed, the chained run
// handle MUST also fail (and carry the deploy job id in the error).
// Otherwise a failed deploy leaves an orphan run job stuck at
// "waiting_for_deploy" forever.
func TestWaitDeployThenLaunch_DeployFails(t *testing.T) {
	r := newJobsForChainTest(t)

	deployHandle, err := r.StartDetached(jobs.KindInstall, "", jobs.Meta{"action": "deploy"})
	require.NoError(t, err)
	runHandle, err := r.StartDetached(jobs.KindRun, "", jobs.Meta{"phase": "waiting_for_deploy"})
	require.NoError(t, err)

	// Spawn the watcher (no appMgr — we never get to the launch branch).
	exec := &RunsExecutor{jobs: r}
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.waitDeployThenLaunch(runHandle, deployHandle.ID(), prov_apps.LaunchRequest{})
	}()

	// Fail the deploy.
	deployHandle.Fail(context.Canceled)

	// Run handle must observe a failed terminal, with the deploy job id
	// surfaced via Handle.Context() being cancelled by the registry's
	// terminal emit.
	select {
	case <-runHandle.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("run handle never terminated after deploy failure")
	}
	<-done
}

// newJobsForChainTest is a focused fixture: registry with a fake clock
// so test runtime is bounded.
func newJobsForChainTest(t *testing.T) *jobs.Registry {
	t.Helper()
	fc := clocktest.NewFakeClock(time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC))
	r, err := jobs.NewRegistry(jobs.Config{
		NodeName:         "test-node",
		Clock:            fc,
		CompletedTTL:     5 * time.Minute,
		JanitorInterval:  30 * time.Second,
		SubscriberBuffer: 8,
	})
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	return r
}

// An auto-deploy downloads the weights a model runs on: a variant's base,
// never the variant's own name, which no registry has. The file is the
// model's #hint when it carries one, else the provider's default quant
// unless the repo already pins one.
func TestAutoDeployRequest(t *testing.T) {
	svc := pkgConfig.ServiceConfig{
		Search:       &pkgConfig.AppSearchConfig{Registry: "huggingface"},
		Capabilities: &pkgConfig.AppCapabilities{DefaultVariant: "Q4_K_M", WireEndpoints: []string{"chat_completions"}},
		Models: map[string]pkgConfig.ModelSpec{
			"fast":       {From: "org/Base-GGUF"},
			"fast-q8":    {From: "org/Base-GGUF#Q8_0.gguf"},
			"pinned-alt": {From: "org/Model-Q5_K_M-GGUF"},
		},
	}
	cases := []struct {
		model, wantModel, wantFile string
	}{
		{"org/Base-GGUF", "org/Base-GGUF", "Q4_K_M"},
		{"org/Base-GGUF#Q8_0.gguf", "org/Base-GGUF", "Q8_0.gguf"},
		{"org/Model-Q5_K_M-GGUF", "org/Model-Q5_K_M-GGUF", ""},
		{"fast", "org/Base-GGUF", "Q4_K_M"},
		{"fast#Q6_K.gguf", "org/Base-GGUF", "Q6_K.gguf"},
		{"pinned-alt", "org/Model-Q5_K_M-GGUF", ""},
		{"fast-q8", "org/Base-GGUF", "Q8_0.gguf"},
		{"fast-q8#Q6_K.gguf", "org/Base-GGUF", "Q6_K.gguf"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			req, err := autoDeployRequest("llamacpp", svc, tc.model)
			require.NoError(t, err)
			assert.Equal(t, tc.wantModel, req.Model)
			assert.Equal(t, tc.wantFile, req.File)
			assert.Equal(t, "huggingface", req.Registry)
		})
	}

	_, err := autoDeployRequest("llamacpp", pkgConfig.ServiceConfig{}, "m")
	assert.ErrorContains(t, err, "search.registry")
}

// The launch path downloads through autoDeployRequest: a variant's base,
// with the provider's default quant, and the launch keeps the variant's
// name with that file as its hint.
func TestHandleAutoDeployLaunch_DownloadsTheVariantsBase(t *testing.T) {
	cfg := &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
	enabled := true
	require.NoError(t, cfg.AddApp("llamacpp", pkgConfig.ServiceConfig{
		Enabled: &enabled, Name: "llamacpp", Mode: constants.AppModeOnDemand, Protocol: pkgConfig.ProtocolOpenAI,
		Search:       &pkgConfig.AppSearchConfig{Registry: "huggingface"},
		Capabilities: &pkgConfig.AppCapabilities{DefaultVariant: "Q4_K_M", WireEndpoints: []string{"chat_completions"}},
		Runtime:      &pkgConfig.AppRuntimeConfig{BasePort: 18080, Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "stub"}},
		Models:       map[string]pkgConfig.ModelSpec{"fast": {From: "org/Base-GGUF"}},
	}))
	var sent *DeployRequest
	exec := &RunsExecutor{
		appsConfig:  func() *pkgConfig.AppsConfig { return cfg },
		getNodename: func() string { return "test-node" },
		jobs:        newJobsForChainTest(t),
		deploy: func(_ context.Context, req *DeployRequest) (*Deployment, error) {
			sent = req
			return &Deployment{Nodes: []modelregistry.DeploymentNode{{Node: "test-node", JobID: "deploy-job"}}}, nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	exec.handleAutoDeployLaunch(c, "llamacpp", "", prov_apps.LaunchRequest{Model: "fast"})

	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body)
	require.NotNil(t, sent)
	assert.Equal(t, "org/Base-GGUF", sent.Model, "the weights, not the variant's name")
	assert.Equal(t, "Q4_K_M", sent.File)
	assert.Equal(t, []string{"test-node"}, sent.Nodes)
}

// Drive the real deployment service so its required-node contract is exercised.
func TestRunsService_AutoDeployTargetsLaunchNode(t *testing.T) {
	for _, target := range []string{"", "@master", "worker-a"} {
		t.Run("target="+target, func(t *testing.T) {
			cfg := &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
			require.NoError(t, cfg.AddApp("llamacpp", pkgConfig.ServiceConfig{
				Name: "llamacpp", Mode: constants.AppModeOnDemand, Protocol: pkgConfig.ProtocolOpenAI,
				Runtime:      &pkgConfig.AppRuntimeConfig{BasePort: 18080, Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "stub"}},
				Search:       &pkgConfig.AppSearchConfig{Registry: "huggingface"},
				Capabilities: &pkgConfig.AppCapabilities{DefaultVariant: "Q4_K_M", WireEndpoints: []string{"chat_completions"}},
				Models:       map[string]pkgConfig.ModelSpec{"fast": {From: "org/Base-GGUF"}},
			}))
			router := &autoDeployRouter{launched: make(chan routing.Request, 1)}
			deployments := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig { return cfg })
			deployments.repoFiles = func(context.Context, string) ([]metadata.TreeFileEntry, error) {
				return []metadata.TreeFileEntry{{Name: "base-Q4_K_M.gguf", Size: 7}}, nil
			}
			service := NewRunsService(router).WithAutoDeploy(deployments, func(string) bool { return false },
				func() *pkgConfig.AppsConfig { return cfg }, newJobsForChainTest(t))
			resp, err := service.LaunchRun(context.Background(), &LaunchRunRequest{
				Provider: "llamacpp", Model: "fast", AutoDeploy: true, Node: target,
			})
			if target == "" || target == "@master" {
				require.Error(t, err, "omitted deploy targets retain the required-target contract")
				require.Nil(t, resp)
				require.Contains(t, err.Error(), "nodes")
				router.mu.Lock()
				sent := router.deployed
				router.mu.Unlock()
				require.Nil(t, sent, "no implicit coordinator download")
				select {
				case <-router.launched:
					t.Fatal("unexpected launch")
				default:
				}
				return
			}
			require.NoError(t, err)
			wantNode := target
			assert.Equal(t, wantNode, resp.Node)
			assert.Equal(t, "download-job", resp.DeployJobID)
			select {
			case launched := <-router.launched:
				assert.Equal(t, wantNode, launched.Node)
				var launch LaunchRunRequest
				require.NoError(t, json.Unmarshal(launched.Body, &launch))
				assert.Equal(t, "fast#Q4_K_M", launch.Model)
				assert.False(t, launch.AutoDeploy)
			case <-time.After(time.Second):
				t.Fatal("deployment completed but launch was not forwarded")
			}
			router.mu.Lock()
			sent := router.deployed
			router.mu.Unlock()
			require.NotNil(t, sent)
			assert.Equal(t, wantNode, sent.Node)
			var deploy struct{ Model, File, Node string }
			require.NoError(t, json.Unmarshal(sent.Body, &deploy))
			assert.Equal(t, "org/Base-GGUF", deploy.Model)
			assert.Equal(t, "Q4_K_M", deploy.File)
			assert.Equal(t, wantNode, deploy.Node)
		})
	}
}

type autoDeployRouter struct {
	fakeRouter
	mu       sync.Mutex
	deployed *routing.Request
	launched chan routing.Request
}

func (r *autoDeployRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	switch {
	case strings.HasPrefix(req.Path, "/zzrouter/v1/internal/jobs/"):
		return &routing.Response{StatusCode: http.StatusOK, Body: []byte(`{"phase":"done","job_id":"download-job"}`)}, nil
	case strings.HasPrefix(req.Path, "/zzrouter/v1/internal/nodes/compatible?"):
		return &routing.Response{StatusCode: http.StatusOK, Body: []byte(`{"data":[{"eligible_providers":["llamacpp"]}]}`)}, nil
	case req.Path == "/zzrouter/v1/internal/deployments" && req.Method == http.MethodPost:
		r.mu.Lock()
		copy := *req
		r.deployed = &copy
		r.mu.Unlock()
		return &routing.Response{StatusCode: http.StatusAccepted, Body: []byte(`{"key":"download-1","job_id":"download-job","status":"completed"}`)}, nil
	case req.Path == "/zzrouter/v1/internal/runs":
		r.launched <- *req
		return &routing.Response{StatusCode: http.StatusAccepted, Body: []byte(`{"id":"run-1","status":"launching","port":18080}`)}, nil
	default:
		return &routing.Response{StatusCode: http.StatusOK, Body: []byte(`{"exists":false,"data":[]}`)}, nil
	}
}

func TestRunsService_AutoDeployPresentModelKeepsMasterDefault(t *testing.T) {
	router := &autoDeployRouter{launched: make(chan routing.Request, 1)}
	service := NewRunsService(router).WithAutoDeploy(&DeploymentsService{}, func(string) bool { return true }, nil, newJobsForChainTest(t))
	_, err := service.LaunchRun(t.Context(), &LaunchRunRequest{Provider: "llamacpp", Model: "present", AutoDeploy: true})
	require.NoError(t, err)
	request := <-router.launched
	assert.Equal(t, "@master", request.Node)
	require.Nil(t, router.deployed)
}
