package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func textCatalogRun(t *testing.T, target *Server) *instance.Instance {
	t.Helper()
	run := instance.NewInstance("text-run", "llamacpp", "weights", 0, 0, 0)
	run.Config = instance.Config{Provider: run.Provider, Model: run.Model}
	run.SetResolved(instance.Resolved{Parameters: map[string]string{}, WireEndpoints: []string{"chat_completions"}})
	run.MarkRunning(false, nil)
	require.NoError(t, target.providers.appMgr.Instances().Register(run))
	return run
}

func TestRunStopPublishesBeforeImageAdmission(t *testing.T) {
	for _, location := range []string{"coordinator", "worker"} {
		for _, routed := range []bool{false, true} {
			t.Run(location+"/routed="+map[bool]string{false: "false", true: "true"}[routed], func(t *testing.T) {
				target, reader, dir := mutationNodes(t, location)
				run := textCatalogRun(t, target)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "mmproj-F16.gguf"), []byte("projector"), 0600))
				require.NoError(t, target.publishCatalog())
				model, found := reader.model.Cache.LookupByNode(run.Model, location)
				require.True(t, found)
				require.Equal(t, "available", model.FeatureState("vision"))
				engine := buildInternalEngine(target)
				path := "/zzrouter/v1/internal/runs/" + run.ID
				if routed {
					public := gin.New()
					ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: engine}), nil, nil, nil, nil)
					public.DELETE("/runs/:id", ctrl.StopRun)
					engine, path = public, "/runs/"+run.ID+"?node="+location
				}
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				model, found = reader.model.Cache.LookupByNode(run.Model, location)
				require.True(t, found)
				require.Equal(t, "present", model.FeatureState("vision"))
				c, admission := mutationContext(t, http.MethodPost, "/v1/chat/completions", nil)
				require.True(t, reader.admitImages(c, run.Model+"@"+location,
					[]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`),
					&resolver.Resolved{ModelName: run.Model, Provider: run.Provider, Node: location}), admission.Body.String())
			})
		}
	}
}

func TestRunMutationPublicationFailure(t *testing.T) {
	for _, operation := range []string{"stop", "restart"} {
		t.Run(operation, func(t *testing.T) {
			target, _, _ := mutationNodes(t, "coordinator")
			run := textCatalogRun(t, target)
			e := target.newRunsExecutor()
			e.publishCatalog = func() error { return errors.New("publication refused") }
			method, path := http.MethodDelete, "/runs/"+run.ID
			var body any
			handler := e.HandleInternalStopRun
			if operation == "restart" {
				method, path, handler = http.MethodPost, path+"/restart", e.HandleInternalRestartRun
				// Relaunch refuses before starting a process, after removing the old evidence.
				run.Config.Provider = "missing"
			}
			if operation == "launch" {
				method, path, handler = http.MethodPost, "/runs", e.HandleInternalLaunchRun
				body = map[string]string{"model_name": run.Model, "provider": run.Provider, "launch_mode": "native"}
			}
			engine := gin.New()
			engine.Handle(method, map[string]string{"stop": "/runs/:id", "restart": "/runs/:id/restart", "launch": "/runs"}[operation], handler)
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			req := httptest.NewRequest(method, path, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "publication refused")
		})
	}
}

type lifecycleRouter struct {
	fakeRouter
	engine http.Handler
}

func (r *lifecycleRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	rec := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, req.Method, req.Path, bytes.NewReader(req.Body))
	request.Header.Set("Content-Type", "application/json")
	r.engine.ServeHTTP(rec, request)
	return &routing.Response{StatusCode: rec.Code, Body: rec.Body.Bytes(), Headers: rec.Header()}, nil
}

func TestRunMutationWaitsForPublication(t *testing.T) {
	for _, operation := range []string{"stop", "restart"} {
		for _, routed := range []bool{false, true} {
			t.Run(operation+"/routed="+fmt.Sprint(routed), func(t *testing.T) {
				target, _, _ := mutationNodes(t, "coordinator")
				run := textCatalogRun(t, target)
				e := target.newRunsExecutor()
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				e.publishCatalog = func() error { close(entered); <-release; return nil }
				method, path, pattern, handler := http.MethodDelete, "/runs/"+run.ID, "/runs/:id", e.HandleInternalStopRun
				var body any
				want := http.StatusOK
				if operation == "restart" {
					method, path, pattern, handler = http.MethodPost, path+"/restart", "/runs/:id/restart", e.HandleInternalRestartRun
					run.Config.Provider = "missing"
					want = http.StatusNotFound // the relaunch names a provider that no longer exists
				}
				if operation == "launch" {
					method, path, pattern, handler = http.MethodPost, "/runs", "/runs", e.HandleInternalLaunchRun
					body = map[string]string{"model_name": run.Model, "provider": run.Provider, "launch_mode": "native"}
					want = http.StatusAccepted
				}
				internal := gin.New()
				internal.Handle(method, "/zzrouter/v1/internal"+pattern, handler)
				engine := internal
				if routed {
					engine = gin.New()
					ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: internal}), nil, nil, nil, nil)
					handlers := map[string]gin.HandlerFunc{"stop": ctrl.StopRun, "restart": ctrl.RestartRun, "launch": ctrl.LaunchRun}
					engine.Handle(method, pattern, handlers[operation])
				} else {
					path = "/zzrouter/v1/internal" + path
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				req := httptest.NewRequest(method, path, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				go func() { defer close(finished); engine.ServeHTTP(rec, req) }()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("mutation skipped publication")
				}
				select {
				case <-finished:
					t.Fatal("mutation answered before publication")
				default:
				}
				close(release)
				<-finished
				require.Equal(t, want, rec.Code, rec.Body.String())
			})
		}
	}
}

// crashingEngineArg makes the re-executed test binary a crashing engine. As a
// positional argument it ends flag parsing, so launch arguments are ignored.
const crashingEngineArg = "crashing-engine"

// TestCrashingEngineHelper is the engine launched by publish-failure tests:
// it exits 23 after the parent releases it following launch acceptance.
func TestCrashingEngineHelper(t *testing.T) {
	if flag.Arg(0) != crashingEngineArg {
		return
	}
	conn, err := net.Dial("tcp", flag.Arg(1))
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, conn)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	os.Exit(23)
}

func TestRunLaunchAndRestartJobsPublishFailure(t *testing.T) {
	for _, operation := range []string{"launch", "restart"} {
		for _, routed := range []bool{false, true} {
			t.Run(operation+"/routed="+fmt.Sprint(routed), func(t *testing.T) {
				target, reader, dir := mutationNodes(t, "worker")
				gate, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				require.NoError(t, err)
				t.Cleanup(func() { _ = gate.Close() })
				binary, err := os.Executable()
				require.NoError(t, err)
				require.NoError(t, target.appsConfig.UpdateApp("llamacpp", func(s *config.ServiceConfig) error {
					// Keep acceptance and the terminal mutation separate.
					s.Runtime.Execution = config.ExecutionConfig{Type: "cli", Command: binary, Args: []string{"-test.run=TestCrashingEngineHelper", crashingEngineArg, gate.Addr().String()}}
					return nil
				}))
				target.providers.appMgr.ReloadConfig(target.appsConfig)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "mmproj-F16.gguf"), []byte("projector"), 0600))
				path, pattern := "/runs", "/runs"
				var body any = map[string]string{"model_name": "weights", "provider": "llamacpp", "launch_mode": "native"}
				e := target.newRunsExecutor()
				handler := e.HandleInternalLaunchRun
				if operation == "restart" {
					run := textCatalogRun(t, target)
					path, pattern, handler, body = path+"/"+run.ID+"/restart", "/runs/:id/restart", e.HandleInternalRestartRun, nil
				}
				internal := gin.New()
				internal.POST("/zzrouter/v1/internal"+pattern, handler)
				engine := internal
				if routed {
					engine = gin.New()
					ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: internal}), nil, nil, nil, nil)
					publicHandler := ctrl.LaunchRun
					if operation == "restart" {
						publicHandler = ctrl.RestartRun
					}
					engine.POST(pattern, publicHandler)
				} else {
					path = "/zzrouter/v1/internal" + path
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, req)
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
				var response struct {
					LaunchRunResponse
					Data LaunchRunResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
				launch := response.LaunchRunResponse
				if routed {
					launch = response.Data
				}
				require.NotEmpty(t, launch.JobID)
				require.NoError(t, gate.SetDeadline(utils.Now().Add(5*time.Second)))
				child, err := gate.Accept()
				require.NoError(t, err)
				require.NoError(t, child.Close())
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				err = (&jobAccess{registry: target.jobs}).Wait(ctx, launch.JobID, "")
				require.Error(t, err)
				ev, err := target.jobs.Get(launch.JobID)
				require.NoError(t, err)
				require.Equal(t, jobs.PhaseFailed, ev.Phase)
				run, exists := target.providers.appMgr.GetInstance(launch.ID)
				require.True(t, exists)
				failure := run.ToInfo("").Failure
				require.NotNil(t, failure)
				require.NotNil(t, failure.ExitCode)
				require.Equal(t, 23, *failure.ExitCode)
				model, found := reader.model.Cache.LookupByNode("weights", "worker")
				require.True(t, found, "the terminal event must leave the coordinator catalog published")
				require.Equal(t, "present", model.FeatureState("vision"))
			})
		}
	}
}

// The auto-deploy chain finishes after the run's own job, which completes only once the catalog is published.
func TestRunAutoDeployChainWaitsForRunJob(t *testing.T) {
	target, _, _ := mutationNodes(t, "coordinator")
	synctest.Test(t, func(t *testing.T) {
		// Close contexts first read by the chain before leaving its bubble.
		t.Cleanup(func() {
			cleanupTestNode(target)
		})
		run := textCatalogRun(t, target)
		child, err := target.jobs.StartDetached(jobs.KindRun, "", nil)
		require.NoError(t, err)
		run.StreamJobID = child.ID()
		deploy, err := target.jobs.StartDetached(jobs.KindInstall, "", nil)
		require.NoError(t, err)
		deploy.Done()
		chain, err := target.jobs.StartDetached(jobs.KindRun, "", nil)
		require.NoError(t, err)
		e := target.newRunsExecutor()
		e.jobs = target.jobs
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			e.waitDeployThenLaunch(chain, deploy.ID(), prov_apps.EnsureRequest{Provider: run.Provider, Model: run.Model})
		}()
		synctest.Wait()
		select {
		case <-finished:
			t.Fatal("auto-deploy chain finished before the run's job")
		default:
		}
		child.Done()
		<-finished
		ev, err := target.jobs.Get(chain.ID())
		require.NoError(t, err)
		require.Equal(t, jobs.PhaseDone, ev.Phase)
	})
}
