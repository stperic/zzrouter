package prov_apps

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// offloadFixture reads placement and ready lines copied verbatim from the offload-step0 captures.
func offloadFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/offload/" + name + ".txt")
	require.NoError(t, err)
	return string(data)
}

func offloadSchema(t *testing.T) *schema.ProviderSchema {
	t.Helper()
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/llamacpp/schema.yaml")
	require.NoError(t, err)
	y, err := schema.LoadYAMLSchema(data)
	require.NoError(t, err)
	declared, errs := schema.Merge("llamacpp", y, config.ServiceConfig{})
	require.Empty(t, errs)
	declared.Parameters["budget"] = schema.ParamShape{} // the contract rig's providers set it
	return declared
}

func offloadWriter(t *testing.T, run *instance.Instance, serve bool) (*health.ReadinessWriter, *health.ReadinessProbe) {
	t.Helper()
	probe := &health.ReadinessProbe{Timeout: time.Second, LogPatterns: health.LogPatterns{
		Success: []health.PatternMatcher{{Pattern: "listening on http"}},
	}}
	if serve {
		probe.Serve = &health.ServeCheck{Path: "/check"}
	}
	require.NoError(t, probe.Validate())
	patterns := offloadSchema(t).Parameters["n-gpu-layers"].PlacementLog
	return health.NewReadinessWriter(io.Discard, probe, []*regexp.Regexp{patterns["layers"], patterns["cpu_overrides"], patterns["weights"]},
		func(index int, values []string) { recordObservation(run, index, values) }), probe
}

func TestPlacementLog(t *testing.T) {
	partial, full, absent := offloadFixture(t, "partial"), offloadFixture(t, "full"), offloadFixture(t, "default")
	override := strings.SplitAfter(offloadFixture(t, "moe_pressure"), "\n")[0]
	for _, tc := range []struct {
		name, log, want string
		split           bool
	}{
		{"MoE pressure", offloadFixture(t, "moe_pressure"), "30% GPU / 70% CPU", false},
		{"MoE alone", offloadFixture(t, "moe_alone"), "100% GPU", false},
		{"dense pressure", offloadFixture(t, "dense27b_pressure"), "64% GPU / 36% CPU", false},
		{"overrides without weights", override + partial, "28% GPU / 72% CPU", false},
		{"MoE split CRLF", strings.ReplaceAll(offloadFixture(t, "moe_pressure"), "\n", "\r\n"), "30% GPU / 70% CPU", true},
		{"partial", partial, "28% GPU / 72% CPU", false},
		{"full", full, "100% GPU", false},
		{"absent", absent, "", false},
		{"last layer observation", strings.SplitAfter(partial, "\n")[0] + full, "100% GPU", false},
		{"split CRLF", strings.ReplaceAll(partial, "\n", "\r\n"), "28% GPU / 72% CPU", true},
		{"CPU", strings.Replace(partial, "8/29", "0/29", 1), "100% CPU", false},
		{"zero total", strings.Replace(partial, "8/29", "0/0", 1), "", false},
		{"invalid then valid", strings.SplitAfter(strings.Replace(partial, "8/29", "0/0", 1), "\n")[0] + full, "100% GPU", false},
		{"GPU exceeds total", strings.Replace(partial, "8/29", "30/29", 1), "", false},
		{"non numeric", strings.Replace(partial, "8/29", "eight/29", 1), "", false},
		{"GPU overflow", strings.Replace(partial, "8/29", "999999999999999999999/29", 1), "", false},
		{"total overflow", strings.Replace(partial, "8/29", "8/999999999999999999999", 1), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := instance.NewInstance("run", "engine", "model", 0, 0, 0)
			writer, _ := offloadWriter(t, run, false)
			chunks := []string{tc.log}
			if tc.split {
				chunks = strings.Split(tc.log, "")
			}
			for _, chunk := range chunks {
				_, err := writer.Write([]byte(chunk))
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, run.ToInfo("").Processor)
			if tc.want == "" {
				assert.Zero(t, run.Loaded().Layers)
			}
		})
	}
}

func TestPlacementWeightsAndOllamaPrecedence(t *testing.T) {
	for _, bytesFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "Ollama bytes last", true: "Ollama bytes first"}[bytesFirst], func(t *testing.T) {
			run := instance.NewInstance("bytes", "engine", "model", 0, 0, 0)
			setBytes := func() { run.UpdateLoaded(func(l *instance.Loaded) { l.Size, l.VRAM = 100, 80 }) }
			if bytesFirst {
				setBytes()
			}
			writer, _ := offloadWriter(t, run, false)
			_, err := writer.Write([]byte(offloadFixture(t, "moe_pressure")))
			require.NoError(t, err)
			loaded := run.Loaded()
			assert.True(t, loaded.CPUOverrides)
			assert.InDelta(t, 17447.91*1024*1024, loaded.CPUWeights, 1)
			assert.InDelta(t, 7651.42*1024*1024, loaded.GPUWeights, 1)
			if !bytesFirst {
				assert.Equal(t, "30% GPU / 70% CPU", run.ToInfo("").Processor)
				setBytes()
			}
			assert.Equal(t, "80% GPU / 20% CPU", run.ToInfo("").Processor)
		})
	}
}

func TestPlacementAccumulatesWeights(t *testing.T) {
	lines := strings.SplitAfter(offloadFixture(t, "moe_pressure"), "\n")
	run := instance.NewInstance("weights", "engine", "model", 0, 0, 0)
	writer, _ := offloadWriter(t, run, false)
	_, err := writer.Write([]byte(strings.Join(lines[:4], "") + lines[2] + lines[3] + lines[3] + lines[4]))
	require.NoError(t, err)
	assert.InDelta(t, 2*17447.91*1024*1024, run.Loaded().CPUWeights, 2)
	assert.InDelta(t, 3*7651.42*1024*1024, run.Loaded().GPUWeights, 3)
	assert.Equal(t, "40% GPU / 60% CPU", run.ToInfo("").Processor)
}

func TestRecordPlacementInvalidValues(t *testing.T) {
	for _, values := range [][]string{{"eight", "29"}, {"8", "many"}, {"-1", "29"}, {"0", "0"}, {"30", "29"}, {"8", "-29"}, {"999999999999999999999", "29"}, {"8", "999999999999999999999"}} {
		t.Run(strings.Join(values, "/"), func(t *testing.T) {
			run := instance.NewInstance("invalid", "engine", "model", 0, 0, 0)
			recordObservation(run, placementLayers, values)
			assert.Empty(t, run.ToInfo("").Processor)
			assert.Zero(t, run.Loaded().Layers)
		})
	}
	for _, value := range []string{"invalid", "1.2.3", "-1", "0", "NaN", "+Inf", "1e300", "1e400"} {
		t.Run("weight/"+value, func(t *testing.T) {
			run := instance.NewInstance("invalid", "engine", "model", 0, 0, 0)
			recordObservation(run, placementWeights, []string{"CPU", value})
			assert.Zero(t, run.Loaded().CPUWeights)
			assert.Empty(t, run.ToInfo("").Processor)
		})
	}
}

func TestPlacementReadinessPublishers(t *testing.T) {
	for _, publisher := range []string{"HTTP first", "log first", "line after running with serve", "line after log readiness"} {
		t.Run(publisher, func(t *testing.T) {
			r := servedContextRig(t, "max_model_len", serviceProbeTransport(func(*http.Request) (*http.Response, error) {
				return contextReply(`{"data":[{"id":"engine-name","max_model_len":4096}]}`), nil
			}))
			run := contextRun(t, r)
			writer, probe := offloadWriter(t, run, publisher == "line after running with serve")
			done := make(chan struct{})
			go func() { defer close(done); r.m.watchReadiness(run, writer, probe) }()
			t.Cleanup(func() { run.Cancel(); waitReadiness(t, done, time.Second) })
			if publisher == "HTTP first" {
				run.HealthURL = "http://localhost:8740/health"
				monitor := health.NewMonitor(r.m.instances, health.MonitorConfig{Interval: time.Millisecond,
					HTTPClient: &http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
						return contextReply(`{}`), nil
					})}})
				monDone := make(chan struct{})
				go func() {
					defer close(monDone)
					monitor.Run(run.Context(), run, health.CheckConfig{HTTPHealthPath: "/health"}, func(current *instance.Instance) { r.m.publishRunning(current, true, "healthy") })
				}()
				t.Cleanup(func() { run.Cancel(); waitReadiness(t, monDone, time.Second) })
				awaitRunning(t, run)
				waitReadiness(t, done, time.Second)
			}
			if strings.HasPrefix(publisher, "line after") {
				_, err := writer.Write([]byte(offloadFixture(t, "default")))
				require.NoError(t, err)
				if probe.Serve != nil {
					require.True(t, r.m.publishRunning(run, true, "healthy"))
				}
				awaitRunning(t, run)
				waitReadiness(t, done, time.Second)
			}
			_, err := writer.Write([]byte(offloadFixture(t, "partial")))
			require.NoError(t, err)
			awaitRunning(t, run)
			waitReadiness(t, done, time.Second)
			drainContextRead(t, run)
			want := "28% GPU / 72% CPU"
			if publisher == "line after log readiness" {
				want = "" // the writer stopped scanning at the ready banner
			}
			assert.Equal(t, want, run.ToInfo("").Processor)
			assert.Equal(t, 4096, run.ToInfo("").ContextLength)
			assert.Len(t, runningPublicationEvents(r.m), 1)
		})
	}
}

func TestPlacementEndingStartup(t *testing.T) {
	for _, ending := range []string{"failure", "stop", "retirement"} {
		for _, captureFirst := range []bool{false, true} {
			t.Run(ending+map[bool]string{false: " before line", true: " after line"}[captureFirst], func(t *testing.T) {
				run := instance.NewInstance("run", "engine", "model", 0, 0, 0)
				writer, _ := offloadWriter(t, run, false)
				line := strings.SplitAfter(offloadFixture(t, "partial"), "\n")[0]
				if captureFirst {
					_, err := writer.Write([]byte(line))
					require.NoError(t, err)
					require.NotEmpty(t, run.ToInfo("").Processor)
				}
				m := &ProviderAppManager{}
				switch ending {
				case "failure":
					m.failNotReady(run, "not ready")
				case "stop":
					require.True(t, run.ClaimEnd())
					run.SetStatus(instance.StatusStopping)
				case "retirement":
					require.True(t, run.ClaimEnd())
				}
				_, err := writer.Write([]byte(line))
				require.NoError(t, err)
				if captureFirst {
					// Placement seen before the end stays: it explains a slow startup.
					assert.Equal(t, "28% GPU / 72% CPU", run.ToInfo("").Processor)
				} else {
					assert.Empty(t, run.ToInfo("").Processor)
					assert.Zero(t, run.Loaded().Layers)
				}
			})
		}
	}
}

func TestServedContextAndPlacementConcurrent(t *testing.T) {
	for _, placementFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "context queued first", true: "placement queued first"}[placementFirst], func(t *testing.T) {
			response, holder := newBarrier(), newBarrier()
			t.Cleanup(response.open)
			t.Cleanup(holder.open)
			r := servedContextRig(t, "max_model_len", serviceProbeTransport(func(*http.Request) (*http.Response, error) {
				response.hold()
				return contextReply(`{"data":[{"id":"engine-name","max_model_len":8192}]}`), nil
			}))
			run := contextRun(t, r)
			var updates sync.WaitGroup
			updates.Go(func() { r.m.readServedContext(run) })
			response.reached(t)
			updates.Go(func() { run.UpdateLoaded(func(*instance.Loaded) { holder.hold() }) })
			holder.reached(t)
			if !placementFirst {
				response.open()
				waitParkedOn(t, ").readServedContext(", 1, contractWait)
			}
			updates.Go(func() { recordObservation(run, placementLayers, []string{"8", "29"}) })
			waitParkedOn(t, "prov_apps.recordObservation(", 1, contractWait)
			if placementFirst {
				response.open()
				waitParkedOn(t, ").readServedContext(", 1, contractWait)
			}
			// Queue both observations behind one lock so a split read/write loses placement.
			holder.open()
			updates.Wait()
			assert.Equal(t, 8192, run.ToInfo("").ContextLength)
			assert.Equal(t, "28% GPU / 72% CPU", run.ToInfo("").Processor)
		})
	}
}

func TestPlacementLaunchAndRestart(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "fit", true: "explicit n-gpu-layers"}[explicit], func(t *testing.T) {
			declared := offloadSchema(t)
			r := newContractRig(t, true, WithProviderSchema(func(string) (*schema.ProviderSchema, error) { return declared, nil }))
			svc, _ := r.m.AppsConfig().LookupApp("generic")
			svc.Runtime.HealthCheck.ReadinessProbe.LogPatterns.Success = []config.PatternConfig{{Pattern: "listening on http"}}
			cfg := &config.AppsConfig{}
			require.NoError(t, cfg.AddApp("generic", svc))
			r.m.ReloadConfig(cfg)
			script := strings.Replace(contractEngine, "printf 'READY\\n'", `cat "$0.output"`, 1)
			require.NoError(t, os.WriteFile(r.engine, []byte(script), 0700))
			require.NoError(t, os.WriteFile(r.engine+".output", []byte(offloadFixture(t, "moe_pressure")), 0600))
			req := EnsureRequest{Provider: "generic", Model: "engine-name"}
			if explicit {
				req.Parameters = map[string]string{"n-gpu-layers": "49"}
			}
			result, err := r.m.EnsureRun(t.Context(), req)
			require.NoError(t, err)
			if explicit {
				assert.Equal(t, "49", result.Run.Resolved().Parameters["n-gpu-layers"])
			}
			r.ready(t)
			awaitRunning(t, result.Run)
			require.EventuallyWithT(t, func(c *assert.CollectT) { assert.Equal(c, "30% GPU / 70% CPU", result.Run.ToInfo("").Processor) }, time.Second, time.Millisecond)
			require.NoError(t, os.Remove(r.engine+".ready"))
			next, err := r.m.RestartInstance(t.Context(), result.Run.ID)
			require.NoError(t, err)
			require.NotSame(t, result.Run, next)
			assert.Empty(t, next.ToInfo("").Processor)
			require.NoError(t, os.WriteFile(r.engine+".output", []byte(offloadFixture(t, "moe_alone")), 0600))
			r.ready(t)
			awaitRunning(t, next)
			require.EventuallyWithT(t, func(c *assert.CollectT) { assert.Equal(c, "100% GPU", next.ToInfo("").Processor) }, time.Second, time.Millisecond)
		})
	}
}

func TestPlacementDeclarationScope(t *testing.T) {
	for _, scenario := range []string{"layers", "other kind", "other parameter", "no diagnostics", "endpoint"} {
		t.Run(scenario, func(t *testing.T) {
			r := newContractRig(t, true)
			svc, _ := r.m.AppsConfig().LookupApp("generic")
			declared := offloadSchema(t)
			want := declared.Parameters["n-gpu-layers"].PlacementLog
			switch scenario {
			case "other kind":
				declared.Diagnostics.Memory.Kind = schema.MemoryUnified
				want = nil
			case "other parameter":
				declared.Diagnostics.Memory.Parameter = "ctx-size"
				want = nil
			case "no diagnostics":
				declared.Diagnostics = nil
				want = nil
			case "endpoint":
				shape := declared.Parameters["n-gpu-layers"]
				shape.PlacementLog = nil
				declared.Endpoints["chat"] = schema.EndpointSchema{Parameters: map[string]schema.ParamShape{"n-gpu-layers": shape}}
				want = nil
			}
			req := EnsureRequest{Provider: "generic", Model: "engine-name"}
			run := instance.NewInstance("scope", "generic", req.Model, 8740, 0, 0)
			run.Endpoint = "chat"
			prepared, err := r.m.prepareRun(t.Context(), run, req, launchSelection{key: "generic", config: svc, model: process.ResolveModel(svc, req.Model)}, declared)
			require.NoError(t, err)
			patterns := make([]*regexp.Regexp, cacheTokens)
			if len(want) > 0 {
				patterns = []*regexp.Regexp{want["layers"], want["cpu_overrides"], want["weights"]}
			}
			assert.Equal(t, patterns, prepared.observations[:cacheTokens])
			delete(declared.Parameters["n-gpu-layers"].PlacementLog, "weights")
			declared.Parameters["n-gpu-layers"] = schema.ParamShape{}
			assert.Equal(t, patterns, prepared.observations[:cacheTokens], "launch preparation freezes the patterns")
		})
	}
}
