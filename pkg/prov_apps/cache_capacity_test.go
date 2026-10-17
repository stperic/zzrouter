package prov_apps

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The partitioned fixture is the verbatim 2026-10-16 lab capture. The iswa and
// repeat fixtures model two-cache output and identical repeated output.
func cacheFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/cache/" + name + ".txt")
	require.NoError(t, err)
	return string(data)
}

func cacheSchema(t *testing.T, provider string) *schema.ProviderSchema {
	t.Helper()
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + provider + "/schema.yaml")
	require.NoError(t, err)
	y, err := schema.LoadYAMLSchema(data)
	require.NoError(t, err)
	declared, errs := schema.Merge(provider, y)
	require.Empty(t, errs)
	return declared
}

func cacheWriter(t *testing.T, run *instance.Instance, provider string, serve bool) *health.ReadinessWriter {
	t.Helper()
	declared := cacheSchema(t, provider)
	patterns := []*regexp.Regexp{
		cacheTokens:        declared.CacheLog["tokens"],
		cacheConcurrency:   declared.CacheLog["concurrency"],
		cachePools:         declared.CacheLog["pools"],
		cacheSlots:         declared.CacheLog["slots"],
		cacheSequenceSlots: declared.CacheLog["sequence_slots"],
	}
	probe := &health.ReadinessProbe{Timeout: time.Second, LogPatterns: health.LogPatterns{
		Success: []health.PatternMatcher{{Pattern: "READY"}}, Failure: []health.PatternMatcher{{Pattern: "FAILED"}},
	}}
	if serve {
		probe.Serve = &health.ServeCheck{Path: "/check"}
	}
	require.NoError(t, probe.Validate())
	return health.NewReadinessWriter(io.Discard, probe, patterns, func(i int, values []string) { recordObservation(run, i, values) })
}

func TestVLLMReportedCacheCapacity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tokens, context int64
		concurrency     float64
	}{
		{"vllm_4096", 251904, 4096, 61.50}, {"vllm_32768", 814421, 32768, 24.85},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
			run.UpdateLoaded(func(l *instance.Loaded) { l.ContextLength = 8192 })
			w := cacheWriter(t, run, "vllm", false)
			_, err := w.Write([]byte(strings.Repeat(cacheFixture(t, tc.name), 2)))
			require.NoError(t, err)
			assert.Equal(t, &instance.CacheCapacity{KVTokens: tc.tokens, ContextTokens: tc.context, MaxConcurrency: tc.concurrency}, run.ToInfo("").Cache)
			assert.Equal(t, 8192, run.ToInfo("").ContextLength)
		})
	}
}

func TestLlamaSharedCacheDoesNotImplyFourFullContexts(t *testing.T) {
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	_, err := cacheWriter(t, run, "llamacpp", false).Write([]byte(cacheFixture(t, "llama_shared")))
	require.NoError(t, err)
	assert.Equal(t, &instance.CacheCapacity{KVTokens: 32768, KVPools: 1, SequenceSlots: 4, ContextTokens: 32768, Sharing: "shared"}, run.ToInfo("").Cache)
}

func TestLlamaPartitionedCachePreservesPerPoolUnits(t *testing.T) {
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	_, err := cacheWriter(t, run, "llamacpp", false).Write([]byte(cacheFixture(t, "llama_partitioned")))
	require.NoError(t, err)
	assert.Equal(t, &instance.CacheCapacity{KVTokens: 32768, KVPools: 4, SequenceSlots: 4, ContextTokens: 32768, Sharing: "partitioned"}, run.ToInfo("").Cache)
}

func TestLlamaPoolCapturesOmitAmbiguousKV(t *testing.T) {
	for _, fixture := range []string{"llama_iswa", "llama_repeat"} {
		t.Run(fixture, func(t *testing.T) {
			run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
			writer := cacheWriter(t, run, "llamacpp", false)
			lines := strings.SplitAfter(cacheFixture(t, fixture), "\n")
			_, err := writer.Write([]byte(lines[0]))
			require.NoError(t, err)
			assert.Equal(t, &instance.CacheCapacity{KVTokens: 32768, KVPools: 1}, run.ToInfo("").Cache)
			_, err = writer.Write([]byte(lines[1]))
			require.NoError(t, err)
			if fixture == "llama_iswa" {
				assert.Nil(t, run.ToInfo("").Cache)
			}
			recordObservation(run, cacheSlots, []string{"4", "32768", "true"})
			// Neither the first nor the second pair can restore ambiguous KV evidence.
			_, err = writer.Write([]byte(cacheFixture(t, fixture)))
			require.NoError(t, err)
			want := &instance.CacheCapacity{SequenceSlots: 4, ContextTokens: 32768, Sharing: "shared"}
			if fixture == "llama_repeat" {
				want.KVTokens, want.KVPools = 32768, 1
			}
			assert.Equal(t, want, run.ToInfo("").Cache)
			raw, err := json.Marshal(run.ToInfo(""))
			require.NoError(t, err)
			var wire struct {
				Cache map[string]any `json:"cache"`
			}
			require.NoError(t, json.Unmarshal(raw, &wire))
			if fixture == "llama_iswa" {
				assert.NotContains(t, wire.Cache, "kv_tokens")
				assert.NotContains(t, wire.Cache, "kv_pools")
			}
		})
	}
	// A different pool count is just as ambiguous as a different cell count.
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	recordObservation(run, cachePools, []string{"32768", "1"})
	recordObservation(run, cachePools, []string{"32768", "4"})
	recordObservation(run, cachePools, []string{"32768", "1"})
	assert.Nil(t, run.ToInfo("").Cache)
}

func TestRecurrentStateCellsAreNotKVTokens(t *testing.T) {
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	for _, line := range strings.Split(cacheFixture(t, "llama_partitioned"), "\n") {
		if strings.Contains(line, "llama_memory_recurrent:") {
			_, err := cacheWriter(t, run, "llamacpp", false).Write([]byte(line + "\n"))
			require.NoError(t, err)
			assert.Nil(t, run.ToInfo("").Cache)
		}
	}
}

func TestMissingCacheEvidenceStaysOmitted(t *testing.T) {
	for _, provider := range []string{"mlx", "vllm", "llamacpp"} {
		t.Run(provider, func(t *testing.T) {
			run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
			_, err := cacheWriter(t, run, provider, false).Write([]byte("Language model ready, continuous batching enabled.\nPrefill completed prompt_tokens=32768\nAvailable KV cache memory: 35.36 GiB\nREADY\n"))
			require.NoError(t, err)
			assert.Nil(t, run.ToInfo("").Cache)
		})
	}
}

func TestCacheCaptureRejectsInvalidNumbers(t *testing.T) {
	for _, value := range []string{"0", "-1", "+1", "1.5", "1e3", "NaN", "Inf", "9223372036854775808", "1,23", "12,,345", ",123", "123,", "1234,567", " 123"} {
		for _, index := range []int{cacheTokens, cacheSequenceSlots} {
			t.Run(value+string(rune('a'+index)), func(t *testing.T) {
				run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
				recordObservation(run, index, []string{value})
				assert.Nil(t, run.ToInfo("").Cache)
			})
		}
	}
	for _, value := range []string{"0", "-1", "NaN", "Inf", "+Inf", "1e400", "invalid"} {
		run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
		recordObservation(run, cacheConcurrency, []string{"32768", value})
		assert.Nil(t, run.ToInfo("").Cache)
	}
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	recordObservation(run, cacheTokens, []string{"9,223,372,036,854,775,807"})
	recordObservation(run, cacheSequenceSlots, []string{"8"})
	assert.Equal(t, int64(9223372036854775807), run.Loaded().Cache.KVTokens)
	assert.Equal(t, int64(8), run.Loaded().Cache.SequenceSlots)
}

func TestCacheCapturePreservesGroupedObservations(t *testing.T) {
	for _, tc := range []struct {
		index   int
		valid   []string
		invalid [][]string
	}{
		{cachePools, []string{"32768", "4"}, [][]string{{"65536", "0"}, {"bad", "2"}}},
		{cacheConcurrency, []string{"32768", "24.85"}, [][]string{{"65536", "NaN"}, {"bad", "4"}}},
		{cacheSlots, []string{"4", "32768", "false"}, [][]string{{"8", "65536", "unknown"}, {"8", "0", "true"}, {"0", "65536", "true"}}},
	} {
		run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
		recordObservation(run, tc.index, tc.valid)
		want := run.Loaded()
		for _, values := range tc.invalid {
			recordObservation(run, tc.index, values)
			assert.Equal(t, want, run.Loaded())
		}
	}
}

func TestCacheCapturesFollowExistingReadinessWindow(t *testing.T) {
	for _, mode := range []string{"split CRLF", "HTTP first", "serve after running", "resolved", "failure", "ending"} {
		t.Run(mode, func(t *testing.T) {
			run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
			writer := cacheWriter(t, run, "vllm", mode == "serve after running")
			switch mode {
			case "HTTP first":
				run.MarkRunning(false, nil)
			case "serve after running":
				_, err := writer.Write([]byte("READY\n"))
				require.NoError(t, err)
				run.MarkRunning(false, nil)
			case "resolved":
				_, err := writer.Write([]byte("READY\n"))
				require.NoError(t, err)
			case "failure":
				_, err := writer.Write([]byte("FAILED\n"))
				require.NoError(t, err)
			case "ending":
				run.MarkFailed("failure")
			}
			log := strings.ReplaceAll(cacheFixture(t, "vllm_32768"), "\n", "\r\n")
			for _, b := range []byte(log) {
				_, err := writer.Write([]byte{b})
				require.NoError(t, err)
			}
			if mode == "resolved" || mode == "failure" || mode == "ending" {
				assert.Nil(t, run.ToInfo("").Cache)
			} else {
				require.NotNil(t, run.ToInfo("").Cache)
				assert.Equal(t, int64(814421), run.Loaded().Cache.KVTokens)
			}
		})
	}
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	writer := cacheWriter(t, run, "vllm", false)
	_, err := writer.Write([]byte(cacheFixture(t, "vllm_32768")))
	require.NoError(t, err)
	run.MarkFailed("startup failed")
	_, err = writer.Write([]byte(cacheFixture(t, "vllm_4096")))
	require.NoError(t, err)
	assert.Equal(t, int64(814421), run.Loaded().Cache.KVTokens)
}

func TestCacheContextAndPlacementUpdatesSurviveTogether(t *testing.T) {
	for _, cacheFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "context first", true: "cache first"}[cacheFirst], func(t *testing.T) {
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
			if !cacheFirst {
				response.open()
				waitParkedOn(t, ").readServedContext(", 1, contractWait)
			}
			updates.Go(func() { recordObservation(run, cachePools, []string{"32768", "4"}) })
			waitParkedOn(t, "prov_apps.recordObservation(", 1, contractWait)
			updates.Go(func() { recordObservation(run, placementLayers, []string{"8", "29"}) })
			waitParkedOn(t, "prov_apps.recordObservation(", 2, contractWait)
			if cacheFirst {
				response.open()
				waitParkedOn(t, ").readServedContext(", 1, contractWait)
			}
			holder.open()
			updates.Wait()
			assert.Equal(t, 8192, run.ToInfo("").ContextLength)
			assert.Equal(t, "28% GPU / 72% CPU", run.ToInfo("").Processor)
			assert.Equal(t, &instance.CacheCapacity{KVTokens: 32768, KVPools: 4}, run.ToInfo("").Cache)
		})
	}
}

func TestCacheCaptureIsIndependentOfMemoryBudget(t *testing.T) {
	for _, kind := range []string{schema.MemoryLayers, schema.MemoryFractionTotal, schema.MemoryUnified, ""} {
		t.Run(kind, func(t *testing.T) {
			declared := cacheSchema(t, "vllm")
			declared.Diagnostics = nil
			if kind != "" {
				declared.Diagnostics = &schema.Diagnostics{Memory: &schema.MemoryBudget{Kind: kind, Parameter: "budget"}}
			}
			r := newContractRig(t, true)
			r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
				return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 100000, FreeMiB: 100000}}, nil
			}
			svc, _ := r.m.AppsConfig().LookupApp("generic")
			req := EnsureRequest{Provider: "generic", Model: "engine-name"}
			run := instance.NewInstance("r", "generic", req.Model, 8740, 0, 0)
			prepared, err := r.m.prepareRun(t.Context(), run, req, launchSelection{key: "generic", config: svc, model: process.ResolveModel(svc, req.Model)}, declared)
			require.NoError(t, err)
			frozen := prepared.observations[cacheTokens]
			delete(declared.CacheLog, "tokens")
			assert.Same(t, frozen, prepared.observations[cacheTokens])
			writer := health.NewReadinessWriter(io.Discard, prepared.health.ReadinessProbe, prepared.observations, func(i int, values []string) { recordObservation(run, i, values) })
			_, err = writer.Write([]byte(cacheFixture(t, "vllm_32768")))
			require.NoError(t, err)
			require.NotNil(t, run.ToInfo("").Cache)
			assert.Equal(t, int64(814421), run.ToInfo("").Cache.KVTokens)
		})
	}
}

func TestRunCapacityDoesNotChangeLaunchDecisions(t *testing.T) {
	r := newContractRig(t, true)
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 100000, FreeMiB: 100000}}, nil
	}
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	req := EnsureRequest{Provider: "generic", Model: "engine-name"}
	declared := cacheSchema(t, "vllm")
	declared.Diagnostics.Memory.Parameter = "budget"
	with := instance.NewInstance("r", "generic", req.Model, 8740, 0, 0)
	prepared, err := r.m.prepareRun(t.Context(), with, req, launchSelection{key: "generic", config: svc, model: process.ResolveModel(svc, req.Model)}, declared)
	require.NoError(t, err)
	declared.CacheLog = nil
	without := instance.NewInstance("r2", "generic", req.Model, 8740, 0, 0)
	baseline, err := r.m.prepareRun(t.Context(), without, req, launchSelection{key: "generic", config: svc, model: process.ResolveModel(svc, req.Model)}, declared)
	require.NoError(t, err)
	assert.Equal(t, baseline.cmd, prepared.cmd)
	assert.Equal(t, baseline.args, prepared.args)
	assert.Equal(t, without.Resolved(), with.Resolved())
	assert.Equal(t, baseline.vars, prepared.vars)
}

func TestRestartStartsWithoutPriorCacheEvidence(t *testing.T) {
	declared := cacheSchema(t, "vllm")
	declared.Diagnostics = nil
	frozen := maps.Clone(declared.CacheLog)
	r := newContractRig(t, true, WithProviderSchema(func(string) (*schema.ProviderSchema, error) { return declared, nil }))
	script := strings.Replace(contractEngine, "printf 'READY\\n'", `cat "$0.output"; printf 'READY\n'`, 1)
	require.NoError(t, os.WriteFile(r.engine, []byte(script), 0700))
	require.NoError(t, os.WriteFile(r.engine+".output", []byte(cacheFixture(t, "vllm_32768")), 0600))
	result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "engine-name"})
	require.NoError(t, err)
	// Launch patterns are frozen even if subsequent schema metadata changes.
	declared.CacheLog = nil
	r.ready(t)
	awaitRunning(t, result.Run)
	require.EventuallyWithT(t, func(c *assert.CollectT) { assert.NotNil(c, result.Run.ToInfo("").Cache) }, time.Second, time.Millisecond)
	require.NoError(t, os.Remove(r.engine+".ready"))
	declared.CacheLog = frozen
	next, err := r.m.RestartInstance(t.Context(), result.Run.ID)
	require.NoError(t, err)
	assert.Nil(t, next.ToInfo("").Cache)
	require.NoError(t, os.WriteFile(r.engine+".output", []byte(cacheFixture(t, "vllm_4096")), 0600))
	r.ready(t)
	awaitRunning(t, next)
	require.EventuallyWithT(t, func(c *assert.CollectT) { assert.Equal(c, int64(251904), next.Loaded().Cache.KVTokens) }, time.Second, time.Millisecond)
}

func TestOllamaRunningReplyDoesNotInventCacheEvidence(t *testing.T) {
	client := &http.Client{Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/ps", req.URL.Path)
		return contextReply(`{"models":[{"name":"model","size":100,"size_vram":75,"context_length":8192}]}`), nil
	})}
	models, err := protocol.NewOllamaProvider(client).ListRunningModels(t.Context(), protocol.NewTarget("http://engine", backend.Engine()).Ready())
	require.NoError(t, err)
	require.Len(t, models, 1)
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	run.SetLoaded(models[0].Loaded)
	assert.Nil(t, run.ToInfo("").Cache)
	assert.Equal(t, 8192, run.ToInfo("").ContextLength)
	assert.Equal(t, "75% GPU / 25% CPU", run.ToInfo("").Processor)
}

func TestCacheCaptureReplacesReportedGroups(t *testing.T) {
	run := instance.NewInstance("r", "engine", "model", 0, 0, 0)
	recordObservation(run, cachePools, []string{"32768", "4"})
	recordObservation(run, cacheSlots, []string{"4", "32768", "false"})
	recordObservation(run, cachePools, []string{"65536", "1"})
	recordObservation(run, cacheSlots, []string{"8", "65536", "true"})
	assert.Equal(t, instance.CacheCapacity{SequenceSlots: 8, ContextTokens: 65536, Sharing: "shared"}, run.Loaded().Cache)
	run = instance.NewInstance("r", "engine", "model", 0, 0, 0)
	writer := cacheWriter(t, run, "vllm", false)
	_, err := writer.Write([]byte(cacheFixture(t, "vllm_4096") + cacheFixture(t, "vllm_32768")))
	require.NoError(t, err)
	assert.Equal(t, instance.CacheCapacity{KVTokens: 814421, ContextTokens: 32768, MaxConcurrency: 24.85}, run.Loaded().Cache)
}
