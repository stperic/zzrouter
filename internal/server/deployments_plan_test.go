package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type deploymentPlanRouter struct {
	fakeRouter
	providers    map[string][]string
	calls        []routing.Request
	fullyPresent map[string]bool
	presentFiles map[string]map[string]bool
}

func TestCompatibleProvidersHonorsExplicitFormat(t *testing.T) {
	e := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "worker"} }, func(string, string, string) bool { return false })
	e.eligibleProviders = func(_, _, format string, _ bool) []string {
		assert.Equal(t, "gguf", format)
		return []string{"llamacpp"}
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", "/nodes/compatible?model=org/model-safetensors&format=gguf", nil)
	e.HandleInternalListCompatibleNodes(c)
	assert.Contains(t, rec.Body.String(), "llamacpp")
}

func (r *deploymentPlanRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	r.calls = append(r.calls, *req)
	u, _ := url.Parse(req.Path)
	var data any
	switch u.Path {
	case "/zzrouter/v1/internal/nodes/compatible":
		data = map[string]any{"data": []map[string]any{{"eligible_providers": r.providers[req.Node]}}}
	case "/zzrouter/v1/internal/sync/exists":
		exists := r.fullyPresent[req.Node]
		if r.presentFiles != nil {
			var request metadata.DownloadRequest
			_ = json.Unmarshal(req.Body, &request)
			exists = true
			for _, f := range request.Files {
				if !r.presentFiles[req.Node][f.Name] {
					exists = false
				}
			}
		}
		data = map[string]any{"exists": exists}
	case "/zzrouter/v1/internal/deployments":
		data = internalDeploymentResult{Key: "download-" + req.Node, JobID: "job-" + req.Node, Status: "downloading"}
	case "/zzrouter/v1/internal/sync/deploy":
		data = syncDeployAsyncResponse{LocalJobID: "sync-" + req.Node}
	default:
		return &routing.Response{StatusCode: 404}, nil
	}
	b, err := json.Marshal(data)
	return &routing.Response{StatusCode: 200, Body: b}, err
}

func TestDeployChecksSourceAgainstEachTargetsPlan(t *testing.T) {
	cfg := featureTestConfig(t)
	text, _ := cfg.LookupApp("llamacpp")
	text.Features = map[string]pkgConfig.Feature{}
	require.NoError(t, cfg.AddApp("llamacpp-text", text))
	router := &deploymentPlanRouter{providers: map[string][]string{"text": {"llamacpp-text"}, "vision": {"llamacpp"}}, presentFiles: map[string]map[string]bool{"text": {"model.gguf": true}}}
	s := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig { return cfg }).WithNodeURLResolver(func(string) string { return "https://peer:9090" })
	s.repoFiles = func(context.Context, string) ([]metadata.TreeFileEntry, error) {
		return []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}, {Name: "mmproj-F16.gguf", Size: 9}}, nil
	}
	_, err := s.Deploy(t.Context(), &DeployRequest{Model: "org/model-GGUF", Nodes: []string{"text", "vision"}})
	require.NoError(t, err)
	for _, call := range router.calls {
		assert.NotEqual(t, "/zzrouter/v1/internal/sync/deploy", call.Path, "weights-only peer cannot supply the vision target")
	}
}

func featureTestConfig(t *testing.T) *pkgConfig.AppsConfig {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := pkgConfig.LoadAppsConfig(dir)
	require.NoError(t, err)
	return cfg
}

func TestDeployPlansDefaultsAndExplicitSelection(t *testing.T) {
	cfg := featureTestConfig(t)
	router := &deploymentPlanRouter{providers: map[string][]string{"worker": {"llamacpp"}}}
	s := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig { return cfg })
	s.repoFiles = func(context.Context, string) ([]metadata.TreeFileEntry, error) {
		return []metadata.TreeFileEntry{{Name: "model-Q8_0.gguf", Size: 7}, {Name: "mmproj-F16.gguf", Size: 9}}, nil
	}
	req := &DeployRequest{Model: "org/model-GGUF", File: "Q8_0", Nodes: []string{"worker"}}
	plans, err := s.planDeploy(context.Background(), req, req.Nodes)
	require.NoError(t, err)
	require.Len(t, plans["worker"].Download.Files, 2)
	assert.Equal(t, "vision", plans["worker"].Download.Files[1].Feature)
	assert.Equal(t, []string{"vision"}, plans["worker"].Download.Optional)
	empty := []string{}
	req.Features = &empty
	plans, err = s.planDeploy(context.Background(), req, req.Nodes)
	require.NoError(t, err)
	require.Len(t, plans["worker"].Download.Files, 1)
	unknown := []string{"imaginary"}
	req.Features = &unknown
	_, err = s.Deploy(context.Background(), req)
	assert.ErrorContains(t, err, "imaginary")
	assert.Empty(t, s.tracker.ListDeployments(false))
}

func TestDeployRefusesAmbiguousOrWrongProviderBeforeStarting(t *testing.T) {
	cfg := featureTestConfig(t)
	router := &deploymentPlanRouter{providers: map[string][]string{"worker": {"llamacpp", "mlx"}}}
	s := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig { return cfg })
	for _, provider := range []string{"", "vllm"} {
		_, err := s.Deploy(context.Background(), &DeployRequest{Model: "org/model", Nodes: []string{"worker"}, Provider: provider, Force: true})
		require.Error(t, err)
		if provider == "" {
			assert.ErrorContains(t, err, "specify provider")
		} else {
			assert.ErrorContains(t, err, "not eligible")
		}
	}
	assert.Empty(t, s.tracker.ListDeployments(false))
}

func TestDeployCarriesPlanAndForceBypassesPresence(t *testing.T) {
	cfg := featureTestConfig(t)
	router := &deploymentPlanRouter{providers: map[string][]string{"a": {"llamacpp"}, "b": {"llamacpp"}}, fullyPresent: map[string]bool{"a": true}}
	s := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig { return cfg }).WithNodeURLResolver(func(string) string { return "https://peer:9090" })
	s.repoFiles = func(context.Context, string) ([]metadata.TreeFileEntry, error) {
		return []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}, {Name: "mmproj-F16.gguf", Size: 9}}, nil
	}
	for _, force := range []bool{false, true} {
		router.calls = nil
		_, err := s.Deploy(context.Background(), &DeployRequest{Model: "org/model-GGUF", Nodes: []string{"a", "b"}, Force: force})
		require.NoError(t, err)
		pulls, syncs, probes := 0, 0, 0
		for _, call := range router.calls {
			u, _ := url.Parse(call.Path)
			switch u.Path {
			case "/zzrouter/v1/internal/sync/exists":
				probes++
				assert.Equal(t, "POST", call.Method)
				var request metadata.DownloadRequest
				require.NoError(t, json.Unmarshal(call.Body, &request))
				assert.Len(t, request.Files, 2)
			case "/zzrouter/v1/internal/deployments", "/zzrouter/v1/internal/sync/deploy":
				if u.Path == "/zzrouter/v1/internal/deployments" {
					pulls++
				} else {
					syncs++
				}
				var body struct {
					Download metadata.DownloadRequest `json:"download"`
					Provider string                   `json:"provider"`
				}
				require.NoError(t, json.Unmarshal(call.Body, &body))
				assert.Len(t, body.Download.Files, 2)
				assert.Equal(t, "llamacpp", body.Provider)
				assert.Equal(t, force, body.Download.Force)
			}
		}
		if force {
			assert.Equal(t, 2, pulls)
			assert.Zero(t, syncs)
			assert.Zero(t, probes)
		} else {
			assert.Equal(t, 1, pulls)
			assert.Equal(t, 1, syncs)
		}
	}
}

func TestFeatureDownloadKeepsRuntimeOutOfFileSelection(t *testing.T) {
	svc := pkgConfig.ServiceConfig{Features: map[string]pkgConfig.Feature{"vision": {Default: true, Runtime: "mlx-vlm", When: "vision_config"}}}
	names, err := selectedFeatures(svc, nil)
	require.NoError(t, err)
	d := featureDownload(&DeployRequest{Model: "org/model"}, svc, names)
	assert.Empty(t, d.Want)
	assert.Empty(t, d.Features)
	assert.Equal(t, []string{"vision"}, d.Optional)
}

func TestFeatureDownloadPreservesVariantHint(t *testing.T) {
	svc := pkgConfig.ServiceConfig{Models: map[string]pkgConfig.ModelSpec{"model+vision": {From: "org/model#Q8_0"}}}
	d := featureDownload(&DeployRequest{Model: "model+vision"}, svc, nil)
	assert.Equal(t, "org/model", d.Repo)
	assert.Equal(t, "Q8_0", d.Weights)
	d = featureDownload(&DeployRequest{Model: "model+vision", File: "Q4_K_M"}, svc, nil)
	assert.Equal(t, "Q4_K_M", d.Weights)
}

func TestDeployPlanKeyDistinguishesObligations(t *testing.T) {
	d := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "b", Size: 2}}, {TreeFileEntry: metadata.TreeFileEntry{Name: "a", Size: 1}}}}
	key := deployPlanKey("p", []string{"vision"}, &d)
	d.Files[0], d.Files[1] = d.Files[1], d.Files[0]
	assert.Equal(t, key, deployPlanKey("p", []string{"vision", "vision"}, &d))
	d.Files[0].Feature = "vision"
	assert.NotEqual(t, key, deployPlanKey("p", []string{"vision"}, &d))
	d.Files[0].Feature = ""
	d.Force = true
	assert.NotEqual(t, key, deployPlanKey("p", []string{"vision"}, &d))
	d.Force = false
	d.Files[0].SHA256 = "changed"
	assert.NotEqual(t, key, deployPlanKey("p", []string{"vision"}, &d))
}

// unreachableRouter fails every request to one node the way a dead peer
// does, and answers others with a fixed status.
type unreachableRouter struct {
	deploymentPlanRouter
	down     string
	statuses map[string]int
}

func (r *unreachableRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	if req.Node == r.down {
		return nil, errors.New("dial tcp: connection refused")
	}
	if status, ok := r.statuses[req.Node]; ok {
		body := `{"type":"about:blank","title":"` + http.StatusText(status) + `","status":` + fmt.Sprint(status) + `,"detail":"from the node"}`
		return &routing.Response{StatusCode: status, Body: []byte(body)}, nil
	}
	return r.deploymentPlanRouter.Route(ctx, req)
}

// A deploy that cannot be planned says whose failure it is: the caller's
// request is a 400, a part of this node not yet available a 503, and a node
// or registry that did not answer a 502.
func TestDeployPlanErrorsSayWhose(t *testing.T) {
	listing := func(err error) func(context.Context, string) ([]metadata.TreeFileEntry, error) {
		return func(context.Context, string) ([]metadata.TreeFileEntry, error) {
			return []metadata.TreeFileEntry{{Name: "model-Q8_0.gguf", Size: 7}}, err
		}
	}
	unknown := []string{"imaginary"}
	cases := []struct {
		name   string
		apps   bool
		files  func(context.Context, string) ([]metadata.TreeFileEntry, error)
		req    DeployRequest
		status int
	}{
		{"unknown feature", true, listing(nil), DeployRequest{Features: &unknown}, http.StatusBadRequest},
		{"wrong provider", true, listing(nil), DeployRequest{Provider: "vllm"}, http.StatusBadRequest},
		{"provider not configured", true, listing(nil), DeployRequest{Nodes: []string{"ghost"}}, http.StatusBadRequest},
		{"cloud registry wants its provider", true, listing(nil), DeployRequest{Registry: "openai", Provider: "llamacpp"}, http.StatusBadRequest},
		{"node refuses the internal call", true, listing(nil), DeployRequest{Nodes: []string{"forbidden"}}, http.StatusBadGateway},
		{"node fails", true, listing(nil), DeployRequest{Nodes: []string{"broken"}}, http.StatusServiceUnavailable},
		{"no such weights", true, listing(nil), DeployRequest{File: "Q2_K"}, http.StatusBadRequest},
		{"no such repository", true, listing(fmt.Errorf("model 'x' %w", huggingface.ErrModelNotFound)), DeployRequest{}, http.StatusBadRequest},
		{"registry unreachable", true, listing(errors.New("dial tcp: i/o timeout")), DeployRequest{}, http.StatusBadGateway},
		{"node unreachable", true, listing(nil), DeployRequest{Nodes: []string{"down"}}, http.StatusBadGateway},
		{"no provider config", false, listing(nil), DeployRequest{Provider: "llamacpp"}, http.StatusServiceUnavailable},
		{"no file planner", true, nil, DeployRequest{}, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := featureTestConfig(t)
			router := &unreachableRouter{
				deploymentPlanRouter: deploymentPlanRouter{providers: map[string][]string{"worker": {"llamacpp"}, "ghost": {"ghost-provider"}}},
				down:                 "down",
				statuses:             map[string]int{"forbidden": http.StatusForbidden, "broken": http.StatusServiceUnavailable},
			}
			s := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig {
				if tc.apps {
					return cfg
				}
				return nil
			})
			s.repoFiles = tc.files
			req := tc.req
			req.Model = "org/model-GGUF"
			if req.Nodes == nil {
				req.Nodes = []string{"worker"}
			}
			_, err := s.Deploy(t.Context(), &req)
			var routed *RoutedError
			require.ErrorAs(t, err, &routed)
			assert.Equal(t, tc.status, routed.StatusCode, err.Error())
		})
	}
}
