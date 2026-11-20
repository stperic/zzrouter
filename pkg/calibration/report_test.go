package calibration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportJSON(t *testing.T) {
	request := testRequest(chatWorkload(1))
	request.StepTimeoutSeconds = new(600)
	pools := []PoolSample{
		{ID: "GPU-example", Kind: "gpu", Vendor: new("nvidia"), TotalMiB: new(int64(80)),
			UsedMiB: new(int64(20)), FreeMiB: new(int64(60)), UtilizationPct: new(70.0), PowerW: new(200.0),
			Holders: []Holder{{PID: 4101, MemoryMiB: new(int64(20)), RunID: new("a")}}},
		{ID: "host", Kind: "host", TotalMiB: new(int64(200)), AvailableMiB: new(int64(150)),
			SwapUsedMiB: new(int64(0)), CPUPct: new(5.0), HostMemoryMiB: map[string]*int64{"a": new(int64(25))}},
	}
	run := RunReport{
		RunID: "a", Provider: "vllm", Model: "model", Endpoint: Chat, OutputForced: true,
		ResolvedParameters: map[string]string{"max-num-seqs": "1"}, ParametersStatus: "current", Overridden: []string{},
		ServedContextTokens: new(1024), MaxConcurrentRequests: new(0), WorkloadRevision: new("chat-v1"),
		ObservedRoles: []string{"running", "waiting", "kv_cache_usage"},
		Cache:         &CacheReport{KVTokens: new(int64(1024)), ContextTokens: new(int64(1024)), MaxConcurrency: new(1.0)},
	}
	targets := map[string]Target{"a": {Run: run, TargetRevision: "original"}}
	start, end := runObservation("original", 2), runObservation("original", 3)
	start.Running, start.Waiting, start.KVCacheUsage, start.CPUPct = new(1), new(0), new(0.92), new(101.0)
	snapshots := []Snapshot{{Pools: pools, Runs: map[string]RunSample{"a": start}},
		{Pools: pools, Runs: map[string]RunSample{"a": end}}}
	m := measured(0, 0, 500*time.Millisecond, 750*time.Millisecond, time.Second, 10, 2)
	observed := stepObservations{snapshots: snapshots, through: epoch.Add(2 * time.Second)}
	step := summarizeStep(0, 2*time.Second, []stepRun{testStepRun(Chat, 1, m)}, observed, map[string]RunSample{}, false, targets)
	report := Report{
		Outcome: verified, StartedAt: epoch, CompletedAt: epoch.Add(2 * time.Second), Request: request,
		SamplingIntervalMS: 500, LargestSampleGapMS: new(640.0), Resources: checkPools(pools, targets),
		Runs: []RunReport{run}, Steps: []StepReport{step},
	}
	body, err := json.Marshal(report)
	require.NoError(t, err)
	require.JSONEq(t, `{
	  "outcome": "verified", "reason": null,
	  "started_at": "2026-11-19T15:00:00Z", "completed_at": "2026-11-19T15:00:02Z",
	  "request": {"node":"gpu-1", "step_timeout_seconds":600,
	    "runs":[{"run_id":"a", "workload":{"input_tokens":10,"output_tokens":2,"concurrency":[1]}}]},
	  "sampling_interval_ms":500, "largest_sample_gap_ms":640,
	  "resources": [
	    {"id":"GPU-example","kind":"gpu","vendor":"nvidia","total_mib":80,
	     "ownership":"verified","holders":[{"pid":4101,"memory_mib":20,"run_id":"a"}]},
	    {"id":"host","kind":"host","total_mib":200}
	  ],
	  "runs": [{
	    "run_id":"a","provider":"vllm","model":"model","endpoint":"chat",
	    "resolved_parameters":{"max-num-seqs":"1"},"parameters_status":"current","overridden":[],
	    "served_context_tokens":1024,"max_concurrent_requests":0,
	    "cache":{"kv_tokens":1024,"context_tokens":1024,"max_concurrency":1},
	    "output_forced":true,"observed_roles":["running","waiting","kv_cache_usage"],"workload_revision":"chat-v1"
	  }],
	  "steps": [{
	    "index":0,"outcome":"verified","duration_ms":2000,"throughput":{"output_tokens_per_second":1},
	    "resources":[
	      {"id":"GPU-example","peak_used_mib":20,"minimum_free_mib":60,
	       "utilization_pct":{"mean":70,"peak":70},"power_w":{"mean":200,"peak":200},"other_holders_peak_mib":0},
	      {"id":"host","minimum_available_mib":150,"peak_swap_used_mib":0,"cpu_pct":{"mean":5,"peak":5}}
	    ],
	    "runs":[{
	      "run_id":"a","outcome":"verified","concurrency":1,
	      "requests":{"requested":1,"completed":1,"failed":0},"public_requests":0,
	      "input_tokens":{"min":10,"max":10},"output_tokens":{"min":2,"max":2},"concurrent_streams_peak":1,
	      "slot_wait_ms":{"p50":0,"p90":0,"p99":0,"min":0,"max":0,"count":1},
	      "time_to_first_token_ms":{"p50":500,"p90":500,"p99":500,"min":500,"max":500,"count":1},
	      "inter_token_latency_ms":{"p50":500,"p90":500,"p99":500,"min":500,"max":500,"count":1},
	      "throughput":{"input_tokens_per_second":10,"output_tokens_per_second":2,"requests_per_second":1},
	      "memory_peak_mib":{"GPU-example":20,"host":25},"cpu_pct":{"mean":101,"peak":101},
	      "engine":{"running_peak":1,"waiting_peak":0,"kv_cache_usage_peak":0.92}
	    }]
	  }]
	}`, string(body))
}

func TestReportNulls(t *testing.T) {
	pools := []PoolSample{
		{ID: "gpu", Kind: "gpu", Holders: []Holder{}},
		{ID: "wddm", Kind: "gpu", Holders: []Holder{{PID: 13}}},
	}
	body, err := json.Marshal(checkPools(pools, nil))
	require.NoError(t, err)
	require.JSONEq(t, `[
	  {"id":"gpu","kind":"gpu","vendor":null,"total_mib":null,"ownership":"verified","holders":[]},
	  {"id":"wddm","kind":"gpu","vendor":null,"total_mib":null,"ownership":"unknown",
	   "holders":[{"pid":13,"memory_mib":null,"run_id":null}]}
	]`, string(body))
	body, err = json.Marshal(stepPools([]Snapshot{{Pools: pools}}, nil))
	require.NoError(t, err)
	require.JSONEq(t, `[
	  {"id":"gpu","peak_used_mib":null,"minimum_free_mib":null,"utilization_pct":null,
	   "power_w":null,"other_holders_peak_mib":0},
	  {"id":"wddm","peak_used_mib":null,"minimum_free_mib":null,"utilization_pct":null,
	   "power_w":null,"other_holders_peak_mib":null}
	]`, string(body))
	body, err = json.Marshal(reason("cancelled"))
	require.NoError(t, err)
	require.JSONEq(t, `{"code":"cancelled","message":"The calibration was cancelled.",
	  "next_action":{"code":null,"tool":null}}`, string(body))
}
