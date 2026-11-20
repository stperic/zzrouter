package calibration

import "time"

// Report is the terminal calibration summary published with the job.
type Report struct {
	Outcome            Outcome      `json:"outcome"`
	Reason             *Reason      `json:"reason"`
	StartedAt          time.Time    `json:"started_at"`
	CompletedAt        time.Time    `json:"completed_at"`
	Request            Request      `json:"request"`
	SamplingIntervalMS int64        `json:"sampling_interval_ms"`
	LargestSampleGapMS *float64     `json:"largest_sample_gap_ms"`
	Resources          []PoolReport `json:"resources"`
	Runs               []RunReport  `json:"runs"`
	Steps              []StepReport `json:"steps"`
}

// PoolReport records each pool at check time.
type PoolReport struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	TotalMiB *int64 `json:"total_mib"`
	*GPUIdentity
}

// GPUIdentity holds the driver and process attribution available at check time.
type GPUIdentity struct {
	Vendor    *string  `json:"vendor"`
	Ownership string   `json:"ownership"`
	Holders   []Holder `json:"holders"`
}

// Holder identifies a process the driver attributes GPU memory to.
type Holder struct {
	PID       int     `json:"pid"`
	MemoryMiB *int64  `json:"memory_mib"`
	RunID     *string `json:"run_id"`
}

// RunReport records the run and its safe settings at check time.
type RunReport struct {
	RunID                 string            `json:"run_id"`
	Provider              string            `json:"provider"`
	Model                 string            `json:"model"`
	Endpoint              string            `json:"endpoint"`
	ResolvedParameters    map[string]string `json:"resolved_parameters"`
	ParametersStatus      string            `json:"parameters_status"`
	Overridden            []string          `json:"overridden"`
	ServedContextTokens   *int              `json:"served_context_tokens"`
	MaxConcurrentRequests *int              `json:"max_concurrent_requests"`
	Cache                 *CacheReport      `json:"cache"`
	OutputForced          bool              `json:"output_forced"`
	ObservedRoles         []string          `json:"observed_roles"`
	WorkloadRevision      *string           `json:"workload_revision"`
}

// CacheReport records the engine's reported KV capacity.
type CacheReport struct {
	KVTokens       *int64   `json:"kv_tokens"`
	ContextTokens  *int64   `json:"context_tokens"`
	MaxConcurrency *float64 `json:"max_concurrency"`
}

// StepReport summarizes the pools and runs that took part in a step.
type StepReport struct {
	Index      int        `json:"index"`
	Outcome    Outcome    `json:"outcome"`
	Reason     *Reason    `json:"reason,omitempty"`
	DurationMS float64    `json:"duration_ms"`
	Throughput StepRates  `json:"throughput"`
	Resources  []PoolStep `json:"resources"`
	Runs       []RunStep  `json:"runs"`
}

// PoolStep summarizes the fields applicable to the pool's kind.
type PoolStep struct {
	ID string `json:"id"`
	*GPUStats
	*SystemStats
}

// GPUStats contains the driver measurements applicable to a GPU pool.
type GPUStats struct {
	PeakUsedMiB         *int64    `json:"peak_used_mib"`
	MinimumFreeMiB      *int64    `json:"minimum_free_mib"`
	UtilizationPct      *MeanPeak `json:"utilization_pct"`
	PowerW              *MeanPeak `json:"power_w"`
	OtherHoldersPeakMiB *int64    `json:"other_holders_peak_mib"`
}

// SystemStats contains measurements for host or unified memory.
type SystemStats struct {
	MinimumAvailableMiB *int64    `json:"minimum_available_mib"`
	PeakSwapUsedMiB     *int64    `json:"peak_swap_used_mib"`
	CPUPct              *MeanPeak `json:"cpu_pct"`
}

// MeanPeak describes the arithmetic mean and peak of known observations.
type MeanPeak struct {
	Mean float64 `json:"mean"`
	Peak float64 `json:"peak"`
}

// RunStep summarizes one active run's workload and observations.
type RunStep struct {
	RunID                 string            `json:"run_id"`
	Outcome               Outcome           `json:"outcome"`
	Reason                *Reason           `json:"reason,omitempty"`
	Concurrency           int               `json:"concurrency"`
	Requests              RequestCounts     `json:"requests"`
	PublicRequests        *uint64           `json:"public_requests"`
	InputTokens           *TokenRange       `json:"input_tokens"`
	OutputTokens          *TokenRange       `json:"output_tokens"`
	ConcurrentStreamsPeak *int              `json:"concurrent_streams_peak"`
	SlotWaitMS            *Percentiles      `json:"slot_wait_ms"`
	TimeToFirstTokenMS    *Percentiles      `json:"time_to_first_token_ms"`
	InterTokenLatencyMS   *Percentiles      `json:"inter_token_latency_ms"`
	Throughput            RunRates          `json:"throughput"`
	MemoryPeakMiB         map[string]*int64 `json:"memory_peak_mib"`
	CPUPct                *MeanPeak         `json:"cpu_pct"`
	Engine                EnginePeak        `json:"engine"`
}

// RequestCounts records the scheduled, successful and errored requests.
type RequestCounts struct {
	Requested int `json:"requested"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// TokenRange describes the bounds of known token counts.
type TokenRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Percentiles describes the nearest ranks of the measured requests.
type Percentiles struct {
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int     `json:"count"`
}

// StepRates uses the step's wall time across all active runs.
type StepRates struct {
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second"`
}

// RunRates uses the window from first send to last completion.
type RunRates struct {
	InputTokensPerSecond  *float64 `json:"input_tokens_per_second"`
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second"`
	RequestsPerSecond     *float64 `json:"requests_per_second"`
}

// EnginePeak contains the peaks of the declared engine observations.
type EnginePeak struct {
	Running      *int     `json:"running_peak"`
	Waiting      *int     `json:"waiting_peak"`
	KVCacheUsage *float64 `json:"kv_cache_usage_peak"`
}
