package process

import (
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSizingUnknownCausesCoverExistingFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]string)
		policy schema.AutoContext
		cause  UnknownCause
	}{
		{"missing native context", func(c map[string]any, p map[string]string) { p["max-model-len"] = "auto" }, rowPolicy(1), ContextUnresolved},
		{"below minimum", func(c map[string]any, p map[string]string) {
			c["max_position_embeddings"] = 2048
			p["max-model-len"] = "auto"
		}, rowPolicy(1), ContextBelowMinimum},
		{"rope scaling", func(c map[string]any, p map[string]string) {
			c["max_position_embeddings"] = 8192
			c["rope_scaling"] = map[string]any{"factor": 2}
			p["max-model-len"] = "auto"
		}, rowPolicy(1), ContextUnresolved},
		{"KV dtype", func(c map[string]any, p map[string]string) { p["cache"] = "mystery" }, rowPolicy(1), KVDtypeUnsupported},
		{"KV block", func(c map[string]any, p map[string]string) { c["head_dim"] = 127; p["cache"] = "q8_0" }, rowPolicy(1), KVDtypeUnsupported},
		{"context override", func(c map[string]any, p map[string]string) {
			c["max_position_embeddings"] = 8192
			p["max-model-len"] = "auto"
			p["override"] = "yes"
		}, rowPolicy(1), ContextUnresolved},
		{"invalid heads", func(c map[string]any, p map[string]string) { c["num_key_value_heads"] = 3 }, rowPolicy(1), MetadataInvalid},
		{"invalid dimension", func(c map[string]any, p map[string]string) { c["head_dim"] = nil }, rowPolicy(1), MetadataInvalid},
		{"model dtype", func(c map[string]any, p map[string]string) { c["torch_dtype"] = "float64" }, rowPolicy(1), ModelUnmodelled},
		{"MLA", func(c map[string]any, p map[string]string) { c["kv_lora_rank"] = 512 }, rowPolicy(1), ModelUnmodelled},
		{"unknown window", func(c map[string]any, p map[string]string) { c["sliding_window"] = 512 }, rowPolicy(1), ModelUnmodelled},
		{"unknown layer", func(c map[string]any, p map[string]string) {
			c["num_hidden_layers"] = 1
			c["layer_types"] = []string{"other"}
		}, rowPolicy(1), ModelUnmodelled},
		{"contradictory layers", func(c map[string]any, p map[string]string) { c["layer_types"] = []string{"full_attention"} }, rowPolicy(1), MetadataInvalid},
		{"overflow", func(c map[string]any, p map[string]string) { c["num_hidden_layers"] = math.MaxInt64 }, rowPolicy(1), ModelUnmodelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := denseAttentionConfig()
			dir, svc, params := needFixture(t, cfg)
			tc.mutate(cfg, params)
			data := fmtValue(cfg)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0600))
			n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, tc.policy, map[string]schema.ParamShape{"cache": {KVCacheDtype: "both"}, "override": {ContextOverride: true}})
			require.False(t, ok)
			assert.Equal(t, tc.cause, n.UnknownCause)
			assert.Equal(t, int64(1024), n.WeightsMiB)
		})
	}
	dir, svc, params := needFixture(t, denseAttentionConfig())
	for _, name := range []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, nil)
	require.False(t, ok)
	assert.Equal(t, WeightsUnavailable, n.UnknownCause)
}

func TestUnknownCausePrecedenceIsDeterministic(t *testing.T) {
	dir, svc, params := needFixture(t, denseAttentionConfig())
	params["cache"] = "unsupported"
	shapes := map[string]schema.ParamShape{"cache": {KVCacheDtype: "both"}}
	for _, data := range []string{"{", `{"num_hidden_layers":1}`, ""} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0600))
		n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
		require.False(t, ok)
		assert.Equal(t, MetadataInvalid, n.UnknownCause)
		assert.Equal(t, int64(1024), n.WeightsMiB)
	}
	require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
	require.False(t, ok)
	assert.Equal(t, MetadataInvalid, n.UnknownCause)
	for _, name := range []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
	n, ok = ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{}, shapes)
	require.False(t, ok)
	assert.Equal(t, WeightsUnavailable, n.UnknownCause)
}

func TestGGUFUnknownCauses(t *testing.T) {
	for _, tc := range []struct {
		arch  string
		extra map[string]int64
		cause UnknownCause
	}{
		{"unsupported", nil, ModelUnmodelled},
		{"llama", map[string]int64{"block_count": 0}, MetadataInvalid},
		{"llama", map[string]int64{"attention.sliding_window": 512}, ModelUnmodelled},
	} {
		t.Run(tc.arch+fmtValue(tc.extra), func(t *testing.T) {
			n := ggufNeed(t, tc.arch, map[string]string{"ctx-size": "4096"}, tc.extra)
			assert.Equal(t, tc.cause, n.UnknownCause)
			assert.Equal(t, int64(16), n.WeightsMiB)
			assert.Zero(t, n.Total())
		})
	}
}

func TestUnknownAssessmentRetainsIndependentEvidence(t *testing.T) {
	for _, tc := range []struct {
		kind, value string
		devices     []gpu.MemoryDevice
		cause       UnknownCause
	}{
		{schema.MemoryFractionTotal, "bad", nil, MemoryEvidenceUnavailable},
		{schema.MemoryFractionTotal, "auto", nil, MemoryEvidenceUnavailable},
		{schema.MemoryExternal, "", nil, BudgetNotSized},
		{schema.MemoryReserveMiB, "10", []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, BudgetNotSized},
	} {
		t.Run(tc.kind+tc.value, func(t *testing.T) {
			n := Need{WeightsMiB: 1024, ContextMode: "numeric", ContextTokens: 32768, UnknownCause: ModelUnmodelled}
			a, _ := AdmitMemoryBudget(schema.MemoryBudget{Kind: tc.kind, Parameter: "budget"}, n, map[string]string{"budget": tc.value}, nil, tc.devices, 0, nil, nil, 0)
			assert.Equal(t, "unknown", a.Status)
			assert.Equal(t, tc.cause, a.UnknownCause)
			assert.Equal(t, n.WeightsMiB, a.WeightsMiB)
			assert.Equal(t, n.ContextTokens, a.ContextTokens)
			assert.Zero(t, a.CapacityMiB)
		})
	}
}

func TestHybridAllocationUnknownUsesExistingBudgetFallback(t *testing.T) {
	dir, svc, params := needFixture(t, hybridConfig())
	params["max-model-len"] = "auto"
	n, ok := ModelMemoryNeed(ResolvedModel{Path: dir}, svc, params, schema.AutoContext{AutoMinContextTokens: 4096}, nil)
	require.False(t, ok)
	require.Equal(t, ModelUnmodelled, n.UnknownCause)
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9800}}
	for _, value := range []string{"auto", "0.9"} {
		params["budget"] = value
		a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, n, params, nil, devices, 0, nil, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, "unknown", a.Status)
		assert.Equal(t, ModelUnmodelled, a.UnknownCause)
		assert.Equal(t, int64(1024), a.WeightsMiB)
		assert.Equal(t, int64(9800), a.CapacityMiB)
		assert.Zero(t, a.RequiredMiB)
		want := "0.7752"
		if value != "auto" {
			want = value
		}
		assert.Equal(t, want, params["budget"])
	}
}

func TestUnknownCauseDoesNotChangeMemoryDecisions(t *testing.T) {
	// GOLDEN values were captured by running these inputs on main at the merge base.
	// Only the former Incomplete flag maps to UnknownCause; unknown amounts are new evidence.
	type golden struct {
		status, placement              string
		required, capacity, affordable int64
		params, environment            map[string]string
		commitments                    map[string]int64
		observation, invalid           bool
		fit                            *MemoryFitError
		class                          httperr.FailureClass
	}
	numeric := Need{WeightsMiB: 2048, KVMiB: 512, OverheadMiB: 2048, ContextMode: "numeric", ContextTokens: 4096, geometry: &attentionConfig{}}
	native := numeric
	native.ContextMode, native.nativeContext = "auto", true
	nativeHeavy := native
	nativeHeavy.WeightsMiB = 6001
	incomplete := Need{WeightsMiB: 1024, UnknownCause: ModelUnmodelled, ContextMode: "numeric", ContextTokens: 4096}
	weightRefusal := incomplete
	weightRefusal.WeightsMiB = 5001
	autoWeightRefusal := incomplete
	autoWeightRefusal.WeightsMiB = 10001
	auto := autoNeed(t, denseAttentionConfig())
	for _, tc := range []struct {
		name, kind, value      string
		need                   Need
		total, free, ram, held int64
		usable                 float64
		want                   golden
	}{
		{
			name: "fraction numeric fit", kind: schema.MemoryFractionTotal, value: "0.6", need: numeric,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "", required: 5069, capacity: 10000, affordable: 0,
				params:      map[string]string{"budget": "0.6"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "fraction numeric sized refusal", kind: schema.MemoryFractionTotal, value: "0.5", need: numeric,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 5069, capacity: 5000, affordable: 0,
				params:      map[string]string{"budget": "0.5"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Sized: true, BudgetKind: "fraction_total", Parameter: "budget", Device: "GPU-A", RequiredMiB: 5069, FreeMiB: 5000},
				class:       "memory",
			},
		},
		{
			name: "fraction numeric reservation refusal", kind: schema.MemoryFractionTotal, value: "0.6", need: numeric,
			total: 10000, free: 5999, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 6000, capacity: 5999, affordable: 0,
				params:      map[string]string{"budget": "0.6"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 6000, FreeMiB: 5999, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction auto rounded boundary", kind: schema.MemoryFractionTotal, value: "auto", need: numeric,
			total: 10000, free: 7117, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "", required: 7117, capacity: 7117, affordable: 0,
				params:      map[string]string{"budget": "0.5069"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "fraction auto margin short", kind: schema.MemoryFractionTotal, value: "auto", need: numeric,
			total: 10000, free: 7116, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 7117, capacity: 7116, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 7117, FreeMiB: 7116, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction numeric incomplete admit", kind: schema.MemoryFractionTotal, value: "0.9", need: incomplete,
			total: 10000, free: 9800, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "0.9"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "fraction numeric incomplete reservation refusal", kind: schema.MemoryFractionTotal, value: "0.9", need: incomplete,
			total: 10000, free: 8999, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 9000, capacity: 8999, affordable: 0,
				params:      map[string]string{"budget": "0.9"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 9000, FreeMiB: 8999, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction numeric weight refusal", kind: schema.MemoryFractionTotal, value: "0.5", need: weightRefusal,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 5001, capacity: 5000, affordable: 0,
				params:      map[string]string{"budget": "0.5"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{BudgetKind: "fraction_total", Parameter: "budget", Device: "reachable memory", RequiredMiB: 5001, FreeMiB: 5000, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction auto incomplete admit", kind: schema.MemoryFractionTotal, value: "auto", need: incomplete,
			total: 10000, free: 9800, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "0.7752"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "fraction auto incomplete margin refusal", kind: schema.MemoryFractionTotal, value: "auto", need: incomplete,
			total: 10000, free: 2048, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 2049, capacity: 2048, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 2049, FreeMiB: 2048, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction auto weight refusal", kind: schema.MemoryFractionTotal, value: "auto", need: autoWeightRefusal,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 10001, capacity: 10000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{BudgetKind: "fraction_total", Parameter: "budget", Device: "reachable memory", RequiredMiB: 10001, FreeMiB: 10000, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction auto context admit", kind: schema.MemoryFractionTotal, value: "auto", need: auto,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "", required: 8244, capacity: 10000, affordable: 16872,
				params:      map[string]string{"budget": "0.7952"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "fraction auto context refusal", kind: schema.MemoryFractionTotal, value: "auto", need: auto,
			total: 7000, free: 7000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 8245, capacity: 7000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 8245, FreeMiB: 7000, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "fraction numeric auto context admit", kind: schema.MemoryFractionTotal, value: "0.95", need: auto,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "", required: 6196, capacity: 10000, affordable: 28128,
				params:      map[string]string{"budget": "0.95"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "fraction invalid", kind: schema.MemoryFractionTotal, value: "bad", need: numeric,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "bad"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				invalid:     true,
			},
		},
		{
			name: "fraction missing observation", kind: schema.MemoryFractionTotal, value: "auto", need: numeric,
			total: 0, free: 0, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				observation: true,
			},
		},
		{
			name: "layers gpu admit", kind: schema.MemoryLayers, value: "auto", need: numeric,
			total: 10000, free: 10000, ram: 20000, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "gpu", required: 5069, capacity: 10000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "layers gpu commitment refusal", kind: schema.MemoryLayers, value: "auto", need: numeric,
			total: 10000, free: 10000, ram: 20000, held: 1000, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 10000, capacity: 9000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{CommitmentOnly: true, Parameter: "budget", Device: "GPU-A", RequiredMiB: 10000, FreeMiB: 9000, Holders: []MemoryHolder{{MemoryMiB: new(int64(1000)), Run: "held", Ownership: "zzrouter", UnusedCommitmentMiB: 1000}}},
				class:       "memory",
			},
		},
		{
			name: "layers offload admit", kind: schema.MemoryLayers, value: "auto", need: numeric,
			total: 4000, free: 4000, ram: 2000, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "offload", required: 5069, capacity: 6000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "layers offload refusal", kind: schema.MemoryLayers, value: "auto", need: numeric,
			total: 4000, free: 4000, ram: 1000, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 5069, capacity: 5000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Sized: true, BudgetKind: "layers", Parameter: "budget", RequiredMiB: 5069, FreeMiB: 5000},
				class:       "memory",
			},
		},
		{
			name: "layers native adaptation admit", kind: schema.MemoryLayers, value: "auto", need: native,
			total: 4000, free: 4000, ram: 1000, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 5069, capacity: 5000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "layers native weight refusal", kind: schema.MemoryLayers, value: "auto", need: nativeHeavy,
			total: 4000, free: 4000, ram: 1000, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 6001, capacity: 5000, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{BudgetKind: "layers", Parameter: "budget", Device: "reachable memory", RequiredMiB: 6001, FreeMiB: 5000, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "layers auto context admit", kind: schema.MemoryLayers, value: "auto", need: auto,
			total: 10000, free: 10000, ram: 20000, held: 0, usable: 0,
			want: golden{
				status: "fit", placement: "gpu", required: 6196, capacity: 10000, affordable: 131072,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "layers missing ram", kind: schema.MemoryLayers, value: "auto", need: numeric,
			total: 4000, free: 4000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 5069, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "auto"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				observation: true,
			},
		},
		{
			name: "unified admit", kind: schema.MemoryUnified, value: "", need: numeric,
			total: 10000, free: 10000, ram: 10000, held: 0, usable: .75,
			want: golden{
				status: "fit", placement: "", required: 5069, capacity: 7500, affordable: 0,
				params:      map[string]string{"budget": ""},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "unified sized refusal", kind: schema.MemoryUnified, value: "", need: numeric,
			total: 10000, free: 10000, ram: 10000, held: 0, usable: .5,
			want: golden{
				status: "no_fit", placement: "", required: 5069, capacity: 5000, affordable: 0,
				params:      map[string]string{"budget": ""},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Sized: true, BudgetKind: "unified", Parameter: "budget", RequiredMiB: 5069, FreeMiB: 5000},
				class:       "memory",
			},
		},
		{
			name: "unified weight refusal", kind: schema.MemoryUnified, value: "", need: autoWeightRefusal,
			total: 10000, free: 10000, ram: 10000, held: 0, usable: .75,
			want: golden{
				status: "no_fit", placement: "", required: 10001, capacity: 10000, affordable: 0,
				params:      map[string]string{"budget": ""},
				environment: map[string]string{},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{BudgetKind: "unified", Parameter: "budget", Device: "reachable memory", RequiredMiB: 10001, FreeMiB: 10000, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "unified auto context admit", kind: schema.MemoryUnified, value: "", need: auto,
			total: 10000, free: 10000, ram: 10000, held: 0, usable: .75,
			want: golden{
				status: "fit", placement: "", required: 6196, capacity: 7500, affordable: 13584,
				params:      map[string]string{"budget": ""},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "reserve admit", kind: schema.MemoryReserveMiB, value: "4096", need: numeric,
			total: 10000, free: 4096, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "4096", "complement": "5.765625"},
				environment: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"},
				commitments: map[string]int64{"GPU-A": 4096},
			},
		},
		{
			name: "reserve refusal", kind: schema.MemoryReserveMiB, value: "4096", need: numeric,
			total: 10000, free: 4095, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "no_fit", placement: "", required: 4096, capacity: 4095, affordable: 0,
				params:      map[string]string{"budget": "4096", "complement": "5.765625"},
				environment: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"},
				commitments: map[string]int64{},
				fit:         &MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 4096, FreeMiB: 4095, Holders: []MemoryHolder{}},
				class:       "memory",
			},
		},
		{
			name: "reserve invalid", kind: schema.MemoryReserveMiB, value: "0", need: numeric,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "0"},
				environment: map[string]string{},
				commitments: map[string]int64{},
				invalid:     true,
			},
		},
		{
			name: "external admit", kind: schema.MemoryExternal, value: "", need: numeric,
			total: 10000, free: 10000, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": ""},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
		{
			name: "external admits without evidence", kind: schema.MemoryExternal, value: "bad", need: autoWeightRefusal,
			total: 0, free: 0, ram: 0, held: 0, usable: 0,
			want: golden{
				status: "unknown", placement: "", required: 0, capacity: 0, affordable: 0,
				params:      map[string]string{"budget": "bad"},
				environment: map[string]string{},
				commitments: map[string]int64{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]string{"budget": tc.value}
			environment := map[string]string{}
			commitments := map[string]int64{}
			var devices []gpu.MemoryDevice
			if tc.total > 0 {
				devices = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: tc.total, FreeMiB: tc.free, Unified: tc.kind == schema.MemoryUnified}}
			}
			var owners []RunRef
			if tc.held > 0 {
				owners = []RunRef{{ID: "held", Commitments: map[string]int64{"GPU-A": tc.held}}}
			}
			budget := schema.MemoryBudget{Kind: tc.kind, Parameter: "budget"}
			if tc.kind == schema.MemoryReserveMiB {
				budget.ComplementParameter = "complement"
			}
			a, err := AdmitMemoryBudget(budget, tc.need, params, environment, devices, tc.ram, owners, commitments, tc.usable)

			assert.Equal(t, tc.want.status, a.Status)
			assert.Equal(t, tc.want.placement, a.Placement)
			if a.Status != "unknown" {
				assert.Equal(t, tc.want.required, a.RequiredMiB)
				assert.Equal(t, tc.want.capacity, a.CapacityMiB)
				assert.Empty(t, a.UnknownCause)
			}
			assert.Equal(t, tc.need.WeightsMiB, a.WeightsMiB)
			assert.Equal(t, tc.want.affordable, a.AffordableContextTokens)
			assert.Equal(t, tc.want.params, params)
			assert.Equal(t, tc.want.environment, environment)
			assert.Equal(t, tc.want.commitments, commitments)
			assert.Equal(t, tc.want.observation, errors.Is(err, ErrMemoryObservation))
			assert.Equal(t, tc.want.invalid, errors.Is(err, ErrMemoryBudgetInvalid))
			var fit *MemoryFitError
			require.Equal(t, tc.want.fit != nil, errors.As(err, &fit))
			assert.Equal(t, tc.want.fit, fit)
			var engine *httperr.EngineFailure
			require.Equal(t, tc.want.class != "", errors.As(err, &engine))
			if engine != nil {
				assert.Equal(t, tc.want.class, engine.Class)
			}
			if tc.want.fit == nil && !tc.want.observation && !tc.want.invalid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestUnknownCapacityUsesExistingFitEvidence(t *testing.T) {
	for _, kind := range []string{schema.MemoryFractionTotal, schema.MemoryLayers, schema.MemoryUnified} {
		for _, fraction := range []string{"0.9", "auto"} {
			t.Run(kind+fraction, func(t *testing.T) {
				budget := schema.MemoryBudget{Kind: kind, Parameter: "budget"}
				devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9800, Unified: kind == schema.MemoryUnified}}
				params := map[string]string{"budget": fraction}
				n := Need{WeightsMiB: 1, KVMiB: 1, geometry: &attentionConfig{}}
				fit, err := AdmitMemoryBudget(budget, n, maps.Clone(params), nil, devices, 20000, nil, nil, .75)
				require.NoError(t, err)
				require.Equal(t, "fit", fit.Status)
				n.UnknownCause = ModelUnmodelled
				n.geometry = nil
				unknown, err := AdmitMemoryBudget(budget, n, maps.Clone(params), nil, devices, 20000, nil, nil, .75)
				require.NoError(t, err)
				assert.Equal(t, "unknown", unknown.Status)
				assert.Equal(t, fit.CapacityMiB, unknown.CapacityMiB)
			})
		}
	}
}

func TestUnknownOffloadCapacityUsesKnownWeights(t *testing.T) {
	for _, ram := range []int64{0, 20000} {
		t.Run(fmtValue(ram), func(t *testing.T) {
			n := Need{WeightsMiB: 11000, WeightsBytes: 11000 << 20, UnknownCause: ModelUnmodelled, ContextMode: "numeric", ContextTokens: 32768}
			a, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryLayers}, n, nil, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, ram, nil, nil, 0)
			require.NoError(t, err, "missing capacity must preserve admission")
			assert.Equal(t, "unknown", a.Status)
			assert.Equal(t, n.WeightsMiB, a.WeightsMiB)
			assert.Equal(t, n.ContextTokens, a.ContextTokens)
			assert.Zero(t, a.RequiredMiB)
			if ram == 0 {
				assert.Zero(t, a.CapacityMiB)
				assert.Equal(t, MemoryEvidenceUnavailable, a.UnknownCause)
			} else {
				assert.Equal(t, int64(30000), a.CapacityMiB)
				assert.Equal(t, ModelUnmodelled, a.UnknownCause)
			}
		})
	}
}

func TestLegacyExplainableAssessmentNormalization(t *testing.T) {
	for _, mode := range []string{"", "numeric"} {
		for _, status := range []string{"fit", "no_fit", "unknown"} {
			a := MemoryAssessment{Status: status, ContextMode: mode, ContextTokens: 4096, RequiredMiB: 17, CapacityMiB: 19}
			a.NormalizeLegacy()
			if mode == "" {
				assert.Equal(t, "unknown", a.Status)
				assert.Equal(t, NodeOutdated, a.UnknownCause)
				assert.Zero(t, a.RequiredMiB)
			} else {
				assert.Equal(t, status, a.Status)
				assert.Equal(t, int64(19), a.CapacityMiB)
				if status == "unknown" {
					assert.Equal(t, NodeOutdated, a.UnknownCause)
				} else {
					assert.Empty(t, a.UnknownCause)
				}
			}
		}
	}
}
