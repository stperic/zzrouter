package prov_apps

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolsLaunchFixture(t *testing.T) (*ProviderAppManager, config.ServiceConfig, string) {
	t.Helper()
	m, svc, dir := featureLaunchFixture(t)
	svc.Features["tools"] = config.Feature{RequiresParameters: []string{"enable-auto-tool-choice", "tool-call-parser"}}
	declared, errs := schema.Merge("vllm", &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{
		"enable-auto-tool-choice": {Type: "bool"}, "tool-call-parser": {Type: "string"}, "chat-template": {Type: "string"},
	}})
	require.Empty(t, errs)
	m.providerSchema = func(string) (*schema.ProviderSchema, error) { return declared, nil }
	return m, svc, dir
}

func reloadToolsConfig(t *testing.T, m *ProviderAppManager, svc config.ServiceConfig) {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
}

func toolsState(m *ProviderAppManager, provider, model string) string {
	return m.ModelFeatureDetails(provider, model, nil)["features"].(map[string]string)["tools"]
}

func TestToolsLaunchRequirements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"neither", nil, "available"},
		{"choice only", map[string]string{"enable-auto-tool-choice": "true"}, "available"},
		{"parser only", map[string]string{"tool-call-parser": "auto"}, "available"},
		{"both", map[string]string{"enable-auto-tool-choice": "true", "tool-call-parser": "chosen"}, "present"},
		{"opt out", map[string]string{"enable-auto-tool-choice": "false", "tool-call-parser": "chosen"}, "available"},
		{"empty parser", map[string]string{"enable-auto-tool-choice": "true", "tool-call-parser": ""}, "available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, svc, _ := toolsLaunchFixture(t)
			svc.Defaults.Parameters = map[string]string{"enable-auto-tool-choice": "true", "tool-call-parser": "default"}
			svc.Models["vendor/model"] = config.ModelSpec{Parameters: map[string]string{"enable-auto-tool-choice": "false"}}
			reloadToolsConfig(t, m, svc)
			assert.Equal(t, "unknown", toolsState(m, "vllm", "vendor/model"))
			if tc.params == nil || len(tc.params) == 1 {
				svc.Defaults.Parameters = nil
				svc.Models["vendor/model"] = config.ModelSpec{}
				reloadToolsConfig(t, m, svc)
			}
			prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model", Parameters: tc.params}, 8100)
			require.NoError(t, err)
			assert.Equal(t, tc.want, prepared.resolved.Features["tools"])
			if tc.name == "both" {
				assert.Contains(t, prepared.launch.Args, "--enable-auto-tool-choice")
				assert.Contains(t, prepared.launch.Args, "chosen")
			}
		})
	}
	t.Run("schema reference", func(t *testing.T) {
		m, svc, _ := toolsLaunchFixture(t)
		svc.Features["tools"] = config.Feature{RequiresParameters: []string{"undeclared"}}
		prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model"}, 8100)
		require.NoError(t, err)
		assert.Equal(t, "unknown", prepared.resolved.Features["tools"])
	})
}

func TestToolsTemplateEvidenceBounds(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"named", `{"chat_template":"tool_use"}`},
		{"malformed", `{"chat_template":`},
		{"oversized", strings.Repeat("tools", 30000)},
		{"unreadable", ""},
		{"missing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, svc, dir := toolsLaunchFixture(t)
			reloadToolsConfig(t, m, svc)
			p := filepath.Join(dir, "tokenizer_config.json")
			if tc.name == "unreadable" {
				require.NoError(t, os.Mkdir(p, 0700))
			} else if tc.name != "missing" {
				require.NoError(t, os.WriteFile(p, []byte(tc.body), 0600))
			}
			prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model", Parameters: map[string]string{"chat-template": "tools-template"}}, 8100)
			require.NoError(t, err)
			assert.Equal(t, "available", prepared.resolved.Features["tools"])
			assert.Equal(t, "unknown", toolsState(m, "vllm", "vendor/model"))
			assert.Equal(t, "unknown", m.ModelFeatureDetails("vllm", "vendor/model", map[string]any{"tools": true})["features"].(map[string]string)["tools"], "source hints cannot override launch-only evidence")
		})
	}
}

func TestToolsServingSnapshot(t *testing.T) {
	m, svc, dir := toolsLaunchFixture(t)
	svc.Defaults.Parameters = map[string]string{"enable-auto-tool-choice": "true", "tool-call-parser": "chosen"}
	svc.Defaults.Endpoints = map[string]config.EndpointOverlay{"embeddings": {Parameters: map[string]string{"enable-auto-tool-choice": "false"}}}
	reloadToolsConfig(t, m, svc)
	prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model"}, 8100)
	require.NoError(t, err)
	run := instance.NewInstance("chat-tools", "vllm", "vendor/model", 8100, 0, 0)
	run.SetResolved(prepared.resolved)
	require.NoError(t, m.instances.Register(run))
	assert.Equal(t, "unknown", toolsState(m, "vllm", "vendor/model"))
	assert.Equal(t, "unknown", toolsState(m, "vllm", "variant"))
	run.MarkRunning(false, nil)
	prepared.resolved.Features["tools"] = "available"
	copy := run.Resolved()
	copy.Features["tools"] = "available"
	assert.Equal(t, "present", toolsState(m, "vllm", "variant"), "shared variants use the chat run key")
	svc.Defaults.Parameters = nil
	svc.Features["tools"] = config.Feature{}
	reloadToolsConfig(t, m, svc)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tokenizer_config.json"), []byte(`{"chat_template":"changed"}`), 0600))
	assert.Equal(t, "present", toolsState(m, "vllm", "vendor/model"))
	assert.Equal(t, "present", toolsState(m, "vllm", "variant"))
	endpoint := instance.NewInstance("embedding-tools", "vllm", "vendor/model", 8101, 0, 0)
	endpoint.Endpoint = "embeddings"
	endpoint.SetResolved(instance.Resolved{Features: map[string]string{"tools": "available"}})
	endpoint.MarkRunning(false, nil)
	require.NoError(t, m.instances.Register(endpoint))
	assert.Equal(t, "present", toolsState(m, "vllm", "vendor/model"))
	require.NoError(t, m.instances.Remove(run.ID))
	assert.Equal(t, "unknown", toolsState(m, "vllm", "vendor/model"), "endpoint evidence cannot substitute for chat")
}

func toolsMetadataRun(t *testing.T, transport http.RoundTripper) (*contractRig, *instance.Instance) {
	t.Helper()
	r := servedContextRig(t, "", transport)
	run := contextRun(t, r)
	run.SetResolved(instance.Resolved{Features: map[string]string{"tools": "unknown"}, FeatureHTTP: map[string]config.FeatureHTTP{"tools": {Path: "/props", Field: "chat_template_caps.supports_tools"}}})
	return r, run
}

func TestToolsEngineMetadataFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		err              error
	}{
		{"present", `{"chat_template_caps":{"supports_tools":true}}`, "present", 200, nil},
		{"negative", `{"chat_template_caps":{"supports_tools":false}}`, "unsupported", 200, nil},
		{"absent", `{}`, "unknown", 200, nil},
		{"null", `{"chat_template_caps":{"supports_tools":null}}`, "unknown", 200, nil},
		{"string", `{"chat_template_caps":{"supports_tools":"true"}}`, "unknown", 200, nil},
		{"parent", `{"chat_template_caps":true}`, "unknown", 200, nil},
		{"malformed", `{`, "unknown", 200, nil},
		{"status", `{"chat_template_caps":{"supports_tools":true}}`, "unknown", 503, nil},
		{"timeout", ``, "unknown", 0, context.DeadlineExceeded},
		{"transport", ``, "unknown", 0, errors.New("unreachable")},
		{"oversized", `{"chat_template_caps":{"supports_tools":true},"padding":"` + strings.Repeat("x", constants.MaxClusterBodySize) + `"}`, "unknown", 200, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			r, run := toolsMetadataRun(t, serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				assert.Equal(t, "/props", req.URL.Path)
				assert.Empty(t, req.Header.Get("Authorization"))
				_, bounded := req.Context().Deadline()
				assert.True(t, bounded)
				if tc.err != nil {
					return nil, tc.err
				}
				resp := contextReply(tc.body)
				resp.StatusCode = tc.status
				return resp, nil
			}))
			require.True(t, r.m.publishRunning(run, true, "ready"))
			drainContextRead(t, run)
			assert.Equal(t, tc.want, run.Resolved().Features["tools"])
			assert.Equal(t, tc.want, toolsState(r.m, "generic", "catalog-name"))
			assert.Equal(t, instance.StatusRunning, run.GetStatus())
			assert.Equal(t, int32(1), calls.Load())
			_, found := r.m.instances.Get(run.ID)
			assert.True(t, found)
		})
	}
	for _, ending := range []string{"retired", "stopped", "unhealthy", "canceled"} {
		t.Run(ending, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			r, run := toolsMetadataRun(t, serviceProbeTransport(func(*http.Request) (*http.Response, error) {
				close(started)
				<-release
				return contextReply(`{"chat_template_caps":{"supports_tools":true}}`), nil
			}))
			require.True(t, r.m.publishRunning(run, true, "ready"))
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("metadata read not started")
			}
			switch ending {
			case "retired":
				require.True(t, run.ClaimEnd())
			case "stopped":
				run.SetStatus(instance.StatusStopped)
			case "unhealthy":
				run.MarkUnhealthy("not serving")
			case "canceled":
				run.Cancel()
			}
			close(release)
			drainContextRead(t, run)
			assert.Equal(t, "unknown", run.Resolved().Features["tools"])
			if ending == "retired" {
				run.ReleaseEnd()
			}
		})
	}
}

func TestToolsMetadataDescriptorSnapshot(t *testing.T) {
	m, svc, _ := toolsLaunchFixture(t)
	svc.Features["tools"] = config.Feature{HTTP: &config.FeatureHTTP{Path: "/props", Field: "chat_template_caps.supports_tools"}}
	prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model"}, 8100)
	require.NoError(t, err)
	run := instance.NewInstance("frozen-tools", "vllm", "vendor/model", 8100, 0, 0)
	run.SetResolved(prepared.resolved)
	clear(prepared.resolved.FeatureHTTP)
	copy := run.Resolved()
	clear(copy.FeatureHTTP)
	assert.Equal(t, map[string]config.FeatureHTTP{"tools": {Path: "/props", Field: "chat_template_caps.supports_tools"}}, run.Resolved().FeatureHTTP)
	assert.Equal(t, map[string]string{"tools": "unknown"}, maps.Clone(run.Resolved().Features))
}

func TestToolsLaunchRuntimeExclusions(t *testing.T) {
	m, svc, dir := toolsLaunchFixture(t)
	svc.Features["vision"] = config.Feature{Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"chat_completions"}, ExcludeParameters: []string{"tool-call-parser"}, Default: true}
	reloadToolsConfig(t, m, svc)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	fake := newFakeInstaller("test-runtime")
	fake.installed.Store(true)
	m.installs.dispatcher.Register("test-runtime", fake)
	prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model", Parameters: map[string]string{"enable-auto-tool-choice": "true", "tool-call-parser": "chosen"}}, 8100)
	require.NoError(t, err)
	assert.Equal(t, "test-runtime", prepared.resolved.Runtime)
	assert.Equal(t, "available", prepared.resolved.Features["tools"])
	assert.NotContains(t, prepared.resolved.Parameters, "tool-call-parser")
	for _, f := range []config.Feature{{}, {Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"chat_completions"}}} {
		svc.Features = map[string]config.Feature{"tools": {}, "vision": f}
		reloadToolsConfig(t, m, svc)
		prepared, err = processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model"}, 8100)
		require.NoError(t, err)
		assert.NotContains(t, prepared.resolved.Features, "tools", "source-only declarations leave catalog evidence intact")
	}
}

func TestLaunchFeatureEvidenceDeclarations(t *testing.T) {
	m, svc, _ := toolsLaunchFixture(t)
	for _, features := range []map[string]config.Feature{nil, {}, {"tools": {}}, {"vision": {HTTP: &config.FeatureHTTP{Path: "/props", Field: "vision"}}}} {
		states, probes := m.launchFeatureEvidence("vllm", features, nil, nil)
		assert.NotContains(t, states, "tools")
		if features["vision"].HTTP != nil {
			assert.Equal(t, map[string]string{"vision": "unknown"}, states)
			assert.Contains(t, probes, "vision")
		} else {
			assert.Empty(t, states)
			assert.Empty(t, probes)
		}
	}
	svc.Features = map[string]config.Feature{"tools": {}}
	reloadToolsConfig(t, m, svc)
	prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", EnsureRequest{Provider: "vllm", Model: "vendor/model"}, 8100)
	require.NoError(t, err)
	run := instance.NewInstance("source-tools", "vllm", "vendor/model", 8100, 0, 0)
	run.SetResolved(prepared.resolved)
	run.MarkRunning(false, nil)
	require.NoError(t, m.instances.Register(run))
	assert.Equal(t, "present", m.ModelFeatureDetails("vllm", "vendor/model", map[string]any{"tools": true})["features"].(map[string]string)["tools"])
}

func TestLaunchFeatureMissingSchemaWarnsOnce(t *testing.T) {
	m, _, _ := toolsLaunchFixture(t)
	log := capturePinLog(t)
	features := map[string]config.Feature{
		"tools":  {RequiresParameters: []string{"missing", "disabled"}, HTTP: &config.FeatureHTTP{Path: "/props", Field: "tools"}},
		"vision": {RequiresParameters: []string{"missing", "other-missing"}},
	}
	for _, provider := range []string{"first", "second", "first", "second"} {
		states, probes := m.launchFeatureEvidence(provider, features, nil, map[string]schema.ParamShape{"disabled": {}})
		assert.Equal(t, map[string]string{"tools": "unknown", "vision": "unknown"}, states)
		assert.Empty(t, probes, "unresolved requirements cannot be rescued by HTTP evidence")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	seen := map[[3]string]int{}
	for _, record := range log.records {
		if record.Message != "Feature evidence requires an unknown parameter" {
			continue
		}
		assert.Equal(t, slog.LevelWarn, record.Level)
		attrs := map[string]string{}
		record.Attrs(func(attr slog.Attr) bool { attrs[attr.Key] = attr.Value.String(); return true })
		seen[[3]string{attrs["provider"], attrs["feature"], attrs["key"]}]++
	}
	for _, provider := range []string{"first", "second"} {
		for _, pair := range [][2]string{{"tools", "missing"}, {"vision", "other-missing"}, {"vision", "missing"}} {
			assert.Equal(t, 1, seen[[3]string{provider, pair[0], pair[1]}])
		}
	}
	assert.Len(t, seen, 6)
}

func TestFeatureMetadataIndependentTimeout(t *testing.T) {
	var featureCtx context.Context
	var calls []string
	r := servedContextRig(t, "max_model_len", serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Path)
		if req.URL.Path == "/props" {
			featureCtx = req.Context()
			<-featureCtx.Done()
			return nil, featureCtx.Err()
		}
		assert.Equal(t, "/v1/models", req.URL.Path)
		assert.NotSame(t, featureCtx, req.Context())
		assert.NoError(t, req.Context().Err(), "feature timeout must leave a fresh context-length budget")
		deadline, bounded := req.Context().Deadline()
		assert.True(t, bounded)
		assert.Greater(t, time.Until(deadline), constants.HTTPShortTimeout/2)
		return contextReply(`{"data":[{"id":"engine-name","max_model_len":8192}]}`), nil
	}))
	run := contextRun(t, r)
	run.SetResolved(instance.Resolved{Features: map[string]string{"tools": "unknown"}, FeatureHTTP: map[string]config.FeatureHTTP{"tools": {Path: "/props", Field: "tools"}}})
	run.MarkRunning(false, nil)
	r.m.readServedContext(run)
	assert.Equal(t, []string{"/props", "/v1/models"}, calls)
	assert.Equal(t, 8192, run.Loaded().ContextLength)
	assert.Equal(t, "unknown", run.Resolved().Features["tools"])
	assert.Equal(t, instance.StatusRunning, run.GetStatus())
}

func TestFeatureMetadataUnavailableUntilRestart(t *testing.T) {
	var calls atomic.Int32
	r, run := toolsMetadataRun(t, serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return contextReply(`{}`), nil
		}
		return contextReply(`{"chat_template_caps":{"supports_tools":true}}`), nil
	}))
	require.True(t, r.m.publishRunning(run, true, "ready"))
	drainContextRead(t, run)
	run.MarkUnhealthy("temporary failure")
	require.True(t, r.m.publishRunning(run, true, "recovered"))
	drainContextRead(t, run)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, "unknown", run.Resolved().Features["tools"])
}
