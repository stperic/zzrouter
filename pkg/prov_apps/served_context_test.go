package prov_apps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func servedContextRig(t *testing.T, field string, transport http.RoundTripper, opts ...Option) *contractRig {
	t.Helper()
	r := newContractRig(t, true, append([]Option{WithHTTPClient(&http.Client{Transport: transport}), WithProviderSchema(func(string) (*schema.ProviderSchema, error) {
		return &schema.ProviderSchema{Parameters: map[string]schema.ParamShape{"context": {Served: field}}}, nil
	})}, opts...)...)
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Defaults.Metadata = &config.AppMetadata{ContextParam: "context"}
	svc.Runtime.Execution.WireModel = config.WireModelName
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", svc))
	r.m.ReloadConfig(cfg)
	return r
}

func contextRun(t *testing.T, r *contractRig) *instance.Instance {
	t.Helper()
	run := instance.NewInstance("context-read", "generic", "catalog-name", 8740, 0, 0)
	run.Config = instance.Config{Provider: "generic", Model: "catalog-name"}
	run.WireModel = "engine-name"
	run.Endpoint = "chat"
	require.NoError(t, r.m.instances.Register(run))
	return run
}

func contextReply(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func drainContextRead(t *testing.T, run *instance.Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, run.WaitForGoroutines(ctx))
}

func TestServedContextRead(t *testing.T) {
	for _, tc := range []struct {
		name, field, body string
		unreachable       bool
		want              int
	}{
		{"vllm", "max_model_len", `{"data":[{"id":"other","max_model_len":1},{"id":"engine-name","max_model_len":70560}]}`, false, 70560},
		{"llamacpp", "meta.n_ctx", `{"data":[{"id":"engine-name","meta":{"n_ctx":262144,"n_ctx_train":999999}}]}`, false, 262144},
		{"unreachable", "max_model_len", "", true, 0},
		{"non JSON", "max_model_len", "not JSON", false, 0},
		{"HTTP failure", "max_model_len", "HTTP failure", false, 0},
		{"model missing", "max_model_len", `{"data":[{"id":"catalog-name","max_model_len":8192}]}`, false, 0},
		{"field missing", "meta.n_ctx", `{"data":[{"id":"engine-name","meta":{"n_ctx_train":8192}}]}`, false, 0},
		{"string", "max_model_len", `{"data":[{"id":"engine-name","max_model_len":"8192"}]}`, false, 0},
		{"fraction", "max_model_len", `{"data":[{"id":"engine-name","max_model_len":8192.5}]}`, false, 0},
		{"zero", "max_model_len", `{"data":[{"id":"engine-name","max_model_len":0}]}`, false, 0},
		{"negative", "max_model_len", `{"data":[{"id":"engine-name","max_model_len":-1}]}`, false, 0},
		{"overflow", "max_model_len", `{"data":[{"id":"engine-name","max_model_len":1e50}]}`, false, 0},
		{"parent not object", "meta.n_ctx", `{"data":[{"id":"engine-name","meta":12}]}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, refreshes atomic.Int32
			r := servedContextRig(t, tc.field, serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				assert.Equal(t, "/v1/models", req.URL.Path)
				assert.Equal(t, "localhost:8740", req.URL.Host)
				assert.Empty(t, req.Header.Get("Authorization"))
				assert.Empty(t, req.Header.Get("X-API-Key"))
				_, bounded := req.Context().Deadline()
				assert.True(t, bounded)
				if tc.unreachable {
					return nil, errors.New("unreachable")
				}
				resp := contextReply(tc.body)
				if tc.name == "HTTP failure" {
					resp.StatusCode = http.StatusServiceUnavailable
				}
				return resp, nil
			}), WithCatalogChanged(func() { refreshes.Add(1) }))
			run := contextRun(t, r)
			run.SetLoaded(instance.Loaded{Size: 100, VRAM: 100, Digest: "keep"})
			require.True(t, r.m.publishRunning(run, true, "ready"))
			drainContextRead(t, run)
			assert.Equal(t, int32(1), calls.Load(), "a ready engine needs no connection probe or retry")
			info := run.ToInfo("")
			assert.Equal(t, tc.want, info.ContextLength)
			assert.Equal(t, instance.StatusRunning, info.Status)
			assert.Empty(t, info.ErrorMessage)
			assert.Equal(t, int64(100), info.SizeBytes)
			assert.Equal(t, "keep", run.Loaded().Digest)
			// Publication refreshed the catalog before the read; a read context refreshes it again.
			details := r.m.ModelFeatureDetails("generic", "catalog-name", nil)
			if tc.want > 0 {
				assert.Equal(t, int32(2), refreshes.Load())
				assert.Equal(t, tc.want, details["context_length"])
			} else {
				assert.Equal(t, int32(1), refreshes.Load())
				assert.NotContains(t, details, "context_length")
			}
		})
	}
}

func TestServedContextReadDoesNotDelayHTTPReadiness(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	client := &http.Client{Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/models" {
			return contextReply(`{}`), nil
		}
		close(started)
		select {
		case <-release:
		case <-req.Context().Done():
		}
		return contextReply(`{"data":[{"id":"engine-name","max_model_len":4096}]}`), nil
	})}
	r := servedContextRig(t, "max_model_len", client.Transport)
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Runtime.HealthCheck = config.HealthcheckConfig{Path: "/health"}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", svc))
	r.m.ReloadConfig(cfg)
	r.m.healthMon = health.NewMonitor(r.m.instances, health.MonitorConfig{Interval: time.Millisecond, HTTPClient: client})
	result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "engine-name"})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("HTTP readiness did not start metadata read")
	}
	assert.Equal(t, instance.StatusRunning, result.Run.GetStatus())
	assert.Zero(t, result.Run.ToInfo("").ContextLength)
}

func TestHealthServeCheckPublishesRunningAndReadsContextOnce(t *testing.T) {
	var modelReads, serveChecks atomic.Int32
	checkedRunning := make(chan struct{}, 1)
	var run *instance.Instance
	client := &http.Client{Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/health":
			if run.GetStatus() == instance.StatusRunning {
				select {
				case checkedRunning <- struct{}{}:
				default:
				}
			}
			return contextReply(`{}`), nil
		case "/v1/chat/completions":
			assert.Equal(t, http.MethodPost, req.Method)
			body, err := io.ReadAll(req.Body)
			assert.NoError(t, err)
			assert.JSONEq(t, `{"model":"engine-name"}`, string(body))
			response := contextReply(`{}`)
			if serveChecks.Add(1) == 1 {
				assert.Equal(t, instance.StatusStarting, run.GetStatus())
				response.StatusCode = http.StatusServiceUnavailable
			}
			return response, nil
		case "/v1/models":
			modelReads.Add(1)
			return contextReply(`{"data":[{"id":"engine-name","max_model_len":8192}]}`), nil
		default:
			t.Errorf("unexpected request: %s", req.URL.Path)
			return contextReply(`{}`), nil
		}
	})}
	r := servedContextRig(t, "max_model_len", client.Transport)
	run = contextRun(t, r)
	run.HealthURL = "http://localhost:8740/health"
	monitor := health.NewMonitor(r.m.instances, health.MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, HTTPClient: client})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("health monitor did not stop")
		}
	}()
	go func() {
		defer close(done)
		monitor.Run(ctx, run, health.CheckConfig{HTTPHealthPath: "/health", ReadinessProbe: &health.ReadinessProbe{
			Serve: &health.ServeCheck{Path: "/v1/chat/completions", Body: `{"model":"${WIRE_MODEL}"}`},
		}}, func(current *instance.Instance) { r.m.publishRunning(current, true, "healthy") })
	}()
	select {
	case <-checkedRunning:
	case <-time.After(time.Second):
		t.Fatal("serve check did not publish readiness")
	}
	drainContextRead(t, run)
	assert.Equal(t, instance.StatusRunning, run.GetStatus())
	assert.Equal(t, []EventType{EventInstanceRunning}, runningPublicationEvents(r.m))
	assert.Equal(t, int32(2), serveChecks.Load())
	assert.Equal(t, int32(1), modelReads.Load())
	assert.Equal(t, 8192, run.ToInfo("").ContextLength)
}

func TestServedContextReadOnceAcrossPublishersAndRecovery(t *testing.T) {
	var calls atomic.Int32
	r := servedContextRig(t, "max_model_len", serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return contextReply(`{"data":[{"id":"engine-name","max_model_len":8192}]}`), nil
	}))
	run := contextRun(t, r)
	var publishers sync.WaitGroup
	for range 2 {
		publishers.Go(func() { r.m.publishRunning(run, true, "ready") })
	}
	publishers.Wait()
	drainContextRead(t, run)
	run.MarkUnhealthy("temporary health failure")
	require.True(t, r.m.publishRunning(run, true, "recovered"))
	drainContextRead(t, run)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, 8192, run.ToInfo("").ContextLength)
}

func TestServedContextReadStoppedOrRetired(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "retired", true: "stopped"}[stop], func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			r := servedContextRig(t, "max_model_len", serviceProbeTransport(func(*http.Request) (*http.Response, error) {
				close(started)
				<-release
				return contextReply(`{"data":[{"id":"engine-name","max_model_len":8192}]}`), nil
			}))
			run := contextRun(t, r)
			require.True(t, r.m.publishRunning(run, true, "ready"))
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("read did not start")
			}
			if stop {
				done := make(chan error, 1)
				go func() { done <- r.m.StopInstance(t.Context(), run.ID) }()
				select {
				case <-run.Context().Done():
				case <-time.After(time.Second):
					t.Fatal("stop did not cancel read")
				}
				unblock()
				require.NoError(t, <-done)
				assert.Equal(t, instance.StatusStopped, run.GetStatus())
			} else {
				require.True(t, run.ClaimEnd())
				unblock()
				drainContextRead(t, run)
				assert.Equal(t, instance.StatusRunning, run.GetStatus())
				run.ReleaseEnd()
			}
			assert.Zero(t, run.ToInfo("").ContextLength)
		})
	}
}
