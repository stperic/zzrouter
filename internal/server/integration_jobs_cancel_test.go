package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func downloadCancelAPI(t *testing.T, e *DeploymentsExecutor, remote bool) *gin.Engine {
	t.Helper()
	owner := NewJobsController(e.jobs, "worker", nil)
	public := gin.New()
	if !remote {
		owner.RegisterPublicRoutes(public.Group("/zzrouter/v1"))
		return public
	}
	worker := gin.New()
	owner.RegisterInternalRoutes(worker.Group("/zzrouter/v1/internal"))
	proxy := &JobsProxy{
		StreamClient: func() (*http.Client, error) {
			return &http.Client{Transport: downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				worker.ServeHTTP(w, r)
				return w.Result(), nil
			})}, nil
		},
		ResolveEndpoint: func(node string) *mesh.Endpoint {
			if node != "worker" {
				return nil
			}
			return &mesh.Endpoint{NodeName: "worker", ClusterURL: "https://worker.invalid"}
		},
		ListPeers: func() []string { return []string{"worker"} },
	}
	coord := NewJobsControllerWithProxy(newTestJobsRegistry(t), "coordinator", nil, proxy)
	coord.RegisterPublicRoutes(public.Group("/zzrouter/v1"))
	return public
}

func startCancelableDownload(t *testing.T, e *DeploymentsExecutor, ctx context.Context, registry string) internalDeploymentResult {
	t.Helper()
	req := internalDownloadRequest{Model: "org/model", Registry: registry, Force: true}
	if registry == constants.RepoHuggingFace {
		req.Download = &metadata.DownloadRequest{Repo: req.Model, Force: true,
			Files: []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "model.gguf", Size: 8}}}}
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	router := gin.New()
	router.POST("/zzrouter/v1/internal/deployments", e.HandleInternalDeploy)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/zzrouter/v1/internal/deployments", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, r)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	var out internalDeploymentResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.NotEmpty(t, out.JobID)
	return out
}

func awaitDownloadSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("download did not reach the expected lifecycle boundary")
	}
}

type jobCancelBody struct {
	ctx                     context.Context
	ready, cancelled, abort chan struct{}
	prefix                  bool
}

func (b *jobCancelBody) Read(p []byte) (int, error) {
	if !b.prefix {
		b.prefix = true
		return copy(p, "part"), nil
	}
	close(b.ready)
	select {
	case <-b.ctx.Done():
		close(b.cancelled)
		return 0, b.ctx.Err()
	case <-b.abort:
		return 0, context.Canceled
	}
}
func (*jobCancelBody) Close() error { return nil }

func TestJobsCancelInFlight_StopsDownload(t *testing.T) {
	for _, registry := range []string{constants.RepoHuggingFace, constants.RepoOllama} {
		for _, remote := range []bool{false, true} {
			for _, hinted := range []bool{false, true} {
				for _, reading := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/remote=%t/hinted=%t/reading=%t", registry, remote, hinted, reading), func(t *testing.T) {
						e := downloadTargetExecutor(t)
						ready, cancelled, abort := make(chan struct{}), make(chan struct{}), make(chan struct{})
						t.Cleanup(func() { close(abort) })
						if registry == constants.RepoHuggingFace {
							e.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
								if reading {
									return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: 8,
										Body: &jobCancelBody{ctx: r.Context(), ready: ready, cancelled: cancelled, abort: abort}, Request: r}, nil
								}
								close(ready)
								select {
								case <-r.Context().Done():
									close(cancelled)
									return nil, r.Context().Err()
								case <-abort:
									return nil, context.Canceled
								}
							}))
						} else {
							upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								if r.URL.Path != "/api/pull" {
									w.WriteHeader(http.StatusNotFound)
									return
								}
								_, _ = io.Copy(io.Discard, r.Body)
								if reading {
									_, _ = io.WriteString(w, "{\"status\":\"pulling\",\"digest\":\"sha256:fixture\",\"total\":8,\"completed\":4}\n")
									w.(http.Flusher).Flush()
								}
								close(ready)
								select {
								case <-r.Context().Done():
									close(cancelled)
								case <-abort:
								}
							}))
							t.Cleanup(upstream.Close)
							// A failing cancellation assertion must still release the fixture server.
							t.Cleanup(func() { upstream.CloseClientConnections() })
							cfg := &config.AppsConfig{}
							provider := config.NewOllamaConnectProvider(constants.AppOllama, upstream.URL, "")
							require.NoError(t, cfg.AddApp(constants.AppOllama, config.ServiceConfig{
								Name: constants.AppOllama, Mode: constants.AppModeExternal, Enabled: provider.Enabled,
								Protocol: provider.Protocol, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL}, Capabilities: provider.Capabilities,
							}))
							e.registry.ReloadConfig(cfg)
							e.appsConfig = func() *config.AppsConfig { return cfg }
							e.ollamaDaemon = func() (*backend.Resolved, error) { return e.registry.OllamaConnector().Daemon() }
						}
						api := downloadCancelAPI(t, e, remote)
						out := startCancelableDownload(t, e, t.Context(), registry)
						awaitDownloadSignal(t, ready)
						if reading {
							sub, err := e.jobs.Subscribe(out.JobID, jobs.SubscribeOptions{})
							require.NoError(t, err)
							defer sub.Unsubscribe()
							deadline := time.After(3 * time.Second)
						progress:
							for {
								select {
								case ev, ok := <-sub.Events():
									require.True(t, ok, "transfer ended before any bytes were read")
									if ev.Bytes != nil && ev.Bytes.Done > 0 {
										break progress
									}
								case <-deadline:
									t.Fatal("transfer never reported read bytes")
								}
							}
						}
						path := "/zzrouter/v1/jobs/" + out.JobID
						if hinted {
							path += "?node=worker"
						}
						w := httptest.NewRecorder()
						api.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, path, nil))
						require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
						require.JSONEq(t, fmt.Sprintf(`{"accepted":true,"job_id":%q}`, out.JobID), w.Body.String())
						awaitDownloadSignal(t, cancelled)
						ev := waitJob(t, e.jobs, out.JobID)
						require.Equal(t, jobs.PhaseFailed, ev.Phase)
						require.Equal(t, "cancelled", ev.Meta["transfer_status"])
						status := e.downloads.GetDownloadStatus(out.Key)
						require.NotNil(t, status)
						require.Equal(t, string(constants.StatusCancelled), status.Status)
						e.registry.Stop()
						final, err := e.jobs.Get(out.JobID)
						require.NoError(t, err)
						require.Equal(t, ev, final, "physical unwind cannot publish a later success")
					})
				}
			}
		}
	}
}

func TestDownloadDetachedLifecycle(t *testing.T) {
	e := downloadTargetExecutor(t)
	entered := make(chan context.Context, 1)
	cancelled, abort := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(abort) })
	e.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		entered <- r.Context()
		select {
		case <-r.Context().Done():
			close(cancelled)
			return nil, r.Context().Err()
		case <-abort:
			return nil, context.Canceled
		}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := startCancelableDownload(t, e, ctx, constants.RepoHuggingFace)
	var upstream context.Context
	select {
	case upstream = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never started")
	}
	cancel()
	require.NoError(t, upstream.Err(), "accepting request must not own the transfer")
	ev, err := e.jobs.Get(out.JobID)
	require.NoError(t, err)
	require.False(t, ev.Phase.IsTerminal())
	e.jobs.Stop()
	awaitDownloadSignal(t, cancelled)
	e.registry.Stop()
}

func TestOllamaDownloadPanicPublishesFailure(t *testing.T) {
	e := downloadTargetExecutor(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{\"status\":\"pulling\",\"digest\":\"layer\",\"total\":8,\"completed\":8}\n{\"status\":\"success\"}\n")
	}))
	t.Cleanup(upstream.Close)
	cfg := &config.AppsConfig{}
	provider := config.NewOllamaConnectProvider(constants.AppOllama, upstream.URL, "")
	require.NoError(t, cfg.AddApp(constants.AppOllama, config.ServiceConfig{
		Name: constants.AppOllama, Mode: constants.AppModeExternal, Enabled: new(true), Protocol: "ollama",
		Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL}, Capabilities: provider.Capabilities,
	}))
	e.registry.ReloadConfig(cfg)
	e.ollamaDaemon = func() (*backend.Resolved, error) { return e.registry.OllamaConnector().Daemon() }
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	e.downloadOllama(c, downloadAdmission{model: "model", registry: constants.RepoOllama, key: "worker/ollama/model",
		options: modelregistry.DownloadOptions{OnComplete: func() error { panic("completion fixture") }}})
	require.Equal(t, http.StatusAccepted, w.Code)
	var out internalDeploymentResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	ev := waitJob(t, e.jobs, out.JobID)
	require.Equal(t, jobs.PhaseFailed, ev.Phase)
	require.Contains(t, ev.Err, "panic: completion fixture")
	require.Eventually(t, func() bool {
		e.jobKeysMu.Lock()
		defer e.jobKeysMu.Unlock()
		return e.jobKeys[out.Key] == ""
	}, time.Second, time.Millisecond)
	require.True(t, modelregistry.IsTerminalStatus(e.downloads.GetDownloadStatus(out.Key).Status))
	e.registry.Stop()
}
