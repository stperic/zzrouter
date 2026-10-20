package schema

import (
	"errors"
	"testing"
	"testing/fstest"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestMergeRejectsReversedRanges(t *testing.T) {
	for _, site := range []string{"flat", "endpoint", "inherited"} {
		t.Run(site, func(t *testing.T) {
			parameter := YAMLParam{Type: "int", Min: new(5.0), Max: new(1.0)}
			y := &YAMLSchema{Parameters: map[string]YAMLParam{"count": parameter}}
			if site != "flat" {
				y.Parameters = nil
				if site == "inherited" {
					y.Parameters = map[string]YAMLParam{"count": {Type: "int", Min: new(5.0)}}
					parameter.Min = nil
				}
				y.Endpoints = map[string]YAMLEndpoint{"embeddings": {Parameters: map[string]YAMLParam{"count": parameter}}}
			}
			_, errs := Merge("custom", y, config.ServiceConfig{})
			require.Len(t, errs, 1)
			require.True(t, errors.Is(errs[0], errInvalidRange))
			require.ErrorContains(t, errs[0], "count.min")
		})
	}
	_, errs := Merge("custom", &YAMLSchema{Parameters: map[string]YAMLParam{"count": {Type: "int", Min: new(5.0), Max: new(5.0)}}}, config.ServiceConfig{})
	require.Empty(t, errs, "equal bounds admit exactly one number")
}

func TestLookup(t *testing.T) {
	shape, ok := Lookup("vllm", "max-model-len")
	if !ok || shape.Kind != ParamInt {
		t.Fatalf("vllm.max-model-len expected int, got %+v ok=%v", shape, ok)
	}
	if _, ok := Lookup("vllm", "ghost-flag"); ok {
		t.Error("unknown flag should not resolve")
	}
	if _, ok := Lookup("no-such-provider", "x"); ok {
		t.Error("unknown provider should not resolve")
	}
}

func TestMerge_YAMLAugmentsGoSpine(t *testing.T) {
	y := &YAMLSchema{
		Provider: "vllm",
		Parameters: map[string]YAMLParam{
			// Long-tail YAML-only flag.
			"max-num-batched-tokens": {Type: "int", Description: "Max tokens processed per iteration"},
			// Go-spine flag enriched with Description (Go left blank — but
			// here vllm.max-model-len already has one; use gpu-memory as
			// the case where YAML provides description detail).
			"gpu-memory-utilization": {Description: "Weights + KV cache fraction"},
		},
	}
	merged, errs := Merge("vllm", y, config.ServiceConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected merge errors: %v", errs)
	}
	if s := merged.Parameters["max-num-batched-tokens"]; s.Kind != ParamInt {
		t.Errorf("max-num-batched-tokens Kind = %q, want int", s.Kind)
	}
	if s := merged.Parameters["max-model-len"]; s.Kind != ParamInt {
		t.Errorf("Go spine lost: max-model-len.Kind = %q", s.Kind)
	}
}

func TestMerge_ShapeConflictFails(t *testing.T) {
	y := &YAMLSchema{
		Provider: "vllm",
		Parameters: map[string]YAMLParam{
			// Go says int; YAML tries to claim string. Must error.
			"max-model-len": {Type: "string"},
		},
	}
	_, errs := Merge("vllm", y, config.ServiceConfig{})
	if len(errs) == 0 {
		t.Fatal("expected shape conflict error")
	}
}

func TestMerge_UnknownTypeFails(t *testing.T) {
	y := &YAMLSchema{
		Provider: "vllm",
		Parameters: map[string]YAMLParam{
			"weird": {Type: "frobnicator"},
		},
	}
	_, errs := Merge("vllm", y, config.ServiceConfig{})
	if len(errs) == 0 {
		t.Fatal("expected parse error for unknown type")
	}
}

// Only providers/<kind>/<name>/schema.yaml is a schema. A flat
// <name>.schema.yaml and anything deeper, such as an operator's asset named
// schema.yaml, are not: assets are named by operators, and a deeper match
// would let one inject parameters into any provider.
func TestLoadFromFS_ReadsOnlyProviderSchemas(t *testing.T) {
	inject := []byte("parameters:\n  injected: {type: string}\n")
	fsys := fstest.MapFS{
		"root/on-demand/vllm/schema.yaml":                   {Data: []byte("parameters:\n  max-num-batched-tokens: {type: int}\n")},
		"root/on-demand/llamacpp/schema.yaml":               {Data: []byte("parameters:\n  chat-template-file: {type: asset}\n")},
		"root/on-demand/mlx.schema.yaml":                    {Data: inject},
		"root/on-demand/llamacpp/assets/schema.yaml":        {Data: inject},
		"root/on-demand/llamacpp/assets/vllm.schema.yaml":   {Data: inject},
		"root/on-demand/llamacpp/assets/broken.schema.yaml": {Data: []byte(":\n:not yaml")},
	}
	merged, errs := LoadFromFS(fsys, "root")
	if len(errs) != 0 {
		t.Fatalf("only provider schemas may be parsed: %v", errs)
	}
	if _, ok := merged["vllm"].Parameters["max-num-batched-tokens"]; !ok {
		t.Error("provider schema.yaml not merged")
	}
	if _, ok := merged["vllm"].Parameters["max-model-len"]; !ok {
		t.Error("Go spine lost when yaml augmented")
	}
	if merged["llamacpp"].Parameters["chat-template-file"].Kind != ParamAsset {
		t.Error("asset kind lost in the walk")
	}
	for name, p := range merged {
		if _, ok := p.Parameters["injected"]; ok {
			t.Errorf("%s gained a parameter from a file that is not its schema", name)
		}
	}
	if _, ok := merged["assets"]; ok {
		t.Error("an asset named schema.yaml was loaded as provider \"assets\"")
	}
}

// A provider named "assets" is an ordinary provider.
func TestLoadFromFS_ProviderNamedAssets(t *testing.T) {
	fsys := fstest.MapFS{
		"root/external/assets/schema.yaml": {Data: []byte("parameters:\n  flag: {type: string}\n")},
	}
	merged, errs := LoadFromFS(fsys, "root")
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if _, ok := merged["assets"].Parameters["flag"]; !ok {
		t.Error("a provider named assets lost its schema")
	}
}

func TestMerge_EndpointsOverlay(t *testing.T) {
	y := &YAMLSchema{
		Provider: "llamacpp",
		Endpoints: map[string]YAMLEndpoint{
			"embeddings": {
				Parameters: map[string]YAMLParam{
					"pooling": {Type: "string", Description: "Pooling strategy",
						Enum: []string{"none", "mean", "cls", "last", "rank"}},
				},
			},
		},
	}
	merged, errs := Merge("llamacpp", y, config.ServiceConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected merge errors: %v", errs)
	}
	ep, ok := merged.Endpoints["embeddings"]
	if !ok {
		t.Fatal("embeddings endpoint missing from merged schema")
	}
	p, ok := ep.Parameters["pooling"]
	if !ok || p.Kind != ParamString {
		t.Fatalf("pooling not merged: %+v ok=%v", p, ok)
	}
	if len(p.Enum) != 5 {
		t.Errorf("enum lost in merge: %v", p.Enum)
	}
}

func TestLoadFromFS_ShapeConflictSurfaces(t *testing.T) {
	fsys := fstest.MapFS{
		"root/on-demand/vllm/schema.yaml": {Data: []byte(
			"provider: vllm\nparameters:\n  max-model-len: {type: string}\n",
		)},
	}
	_, errs := LoadFromFS(fsys, "root")
	if len(errs) == 0 {
		t.Fatal("expected conflict surfaced through FS walker")
	}
}

// TestLoadFromFS_EndpointsSurviveTheWalk pins the round trip that Merge's
// unit test cannot see: an endpoint overlay declared on disk has to survive
// the walk, the per-file merge and the keyed result, not just Merge itself.
func TestLoadFromFS_EndpointsSurviveTheWalk(t *testing.T) {
	fsys := fstest.MapFS{
		"root/on-demand/llamacpp/schema.yaml": {Data: []byte(
			"provider: llamacpp\n" +
				"endpoints:\n" +
				"  embeddings:\n" +
				"    parameters:\n" +
				"      pooling: {type: string, enum: [none, mean, cls, last, rank]}\n",
		)},
	}
	merged, errs := LoadFromFS(fsys, "root")
	if len(errs) != 0 {
		t.Fatalf("unexpected load errors: %v", errs)
	}
	ps, ok := merged["llamacpp"]
	if !ok {
		t.Fatal("llamacpp missing from merged result")
	}
	ep, ok := ps.Endpoints["embeddings"]
	if !ok {
		t.Fatalf("embeddings overlay lost in the walk; endpoints=%v", ps.Endpoints)
	}
	p, ok := ep.Parameters["pooling"]
	if !ok || p.Kind != ParamString {
		t.Fatalf("pooling not loaded: %+v ok=%v", p, ok)
	}
	if len(p.Enum) != 5 {
		t.Errorf("enum lost between disk and merged schema: %v", p.Enum)
	}
}

func TestMerge_AssetKind(t *testing.T) {
	y := &YAMLSchema{Parameters: map[string]YAMLParam{
		"chat-template-file": {Type: "asset", Description: "Jinja chat template"},
		// An asset claim on a key the Go spine types as int is still a conflict.
		"ctx-size": {Type: "asset"},
	}}
	merged, errs := Merge("llamacpp", y, config.ServiceConfig{})
	if merged.Parameters["chat-template-file"].Kind != ParamAsset {
		t.Fatalf("chat-template-file: got %+v", merged.Parameters["chat-template-file"])
	}
	if len(errs) != 1 || merged.Parameters["ctx-size"].Kind != ParamInt {
		t.Fatalf("expected one conflict and the spine kind kept, got errs=%v shape=%+v", errs, merged.Parameters["ctx-size"])
	}
}

func TestForEndpoint(t *testing.T) {
	p := &ProviderSchema{
		Parameters: map[string]ParamShape{"a": {Kind: ParamString}, "b": {Kind: ParamString}},
		Endpoints:  map[string]EndpointSchema{"embeddings": {Parameters: map[string]ParamShape{"b": {Kind: ParamAsset}}}},
	}
	if got := p.ForEndpoint("embeddings"); got["b"].Kind != ParamAsset || got["a"].Kind != ParamString {
		t.Errorf("overlay must win and flat keys stay visible: %+v", got)
	}
	if got := p.ForEndpoint("chat"); got["b"].Kind != ParamString {
		t.Errorf("an endpoint without an overlay gets the flat set: %+v", got)
	}
	if p.Parameters["b"].Kind != ParamString {
		t.Error("ForEndpoint must not mutate the flat set")
	}
}

// negatable is a bool's attribute; on any other type it would make false
// render a flag the engine does not have.
func TestMerge_Negatable(t *testing.T) {
	merged, errs := Merge("llamacpp", &YAMLSchema{Parameters: map[string]YAMLParam{
		"mmap":    {Type: "bool", Negatable: true},
		"cpu-moe": {Type: "bool"},
	}}, config.ServiceConfig{})
	if len(errs) != 0 {
		t.Fatalf("unexpected merge errors: %v", errs)
	}
	if !merged.Parameters["mmap"].Negatable || merged.Parameters["cpu-moe"].Negatable {
		t.Errorf("negatable did not merge as declared: %+v", merged.Parameters)
	}
	y, err := LoadYAMLSchema([]byte("parameters:\n  threads: {type: int, negatable: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, errs := Merge("llamacpp", y, config.ServiceConfig{}); len(errs) == 0 {
		t.Error("negatable on an int must be refused")
	}
}

func TestMergeLiteralFlagSpelling(t *testing.T) {
	for _, endpoint := range []string{"", "embeddings"} {
		t.Run(endpoint, func(t *testing.T) {
			parameters := map[string]YAMLParam{"fp8_e4m3fn-unet": {Type: "bool", Negatable: true}}
			y := &YAMLSchema{Parameters: parameters}
			if endpoint != "" {
				y = &YAMLSchema{Endpoints: map[string]YAMLEndpoint{endpoint: {Parameters: parameters}}}
			}
			merged, errs := Merge("custom", y, config.ServiceConfig{})
			require.Empty(t, errs)
			shapes := merged.ForEndpoint(endpoint)
			require.NotContains(t, shapes, "fp8_e4m3fn-unet")
			require.Equal(t, "fp8_e4m3fn-unet", shapes["fp8-e4m3fn-unet"].Flag)
			require.Nil(t, Check("fp8_e4m3fn_unet", "true", shapes))
			parameters["fp8-e4m3fn-unet"] = YAMLParam{Type: "bool"}
			_, errs = Merge("custom", y, config.ServiceConfig{})
			require.NotEmpty(t, errs)
		})
	}
}

func TestEndpointOverlayKeepsBaseLiteralFlag(t *testing.T) {
	merged, errs := Merge("custom", &YAMLSchema{
		Parameters: map[string]YAMLParam{"fp8_e4m3fn-unet": {Type: "bool", Negatable: true}},
		Endpoints: map[string]YAMLEndpoint{"embeddings": {Parameters: map[string]YAMLParam{
			"fp8-e4m3fn-unet": {Type: "bool"},
		}}},
	}, config.ServiceConfig{})
	require.Empty(t, errs)
	shape := merged.ForEndpoint("embeddings")["fp8-e4m3fn-unet"]
	require.Equal(t, "fp8_e4m3fn-unet", shape.Flag)
	require.True(t, shape.Negatable)
}

func TestEndpointOverlayErrorPath(t *testing.T) {
	_, errs := Merge("llamacpp", &YAMLSchema{Endpoints: map[string]YAMLEndpoint{
		"embeddings": {Parameters: map[string]YAMLParam{"ctx-size": {Type: "string"}}},
	}}, config.ServiceConfig{})
	require.Len(t, errs, 1)
	require.EqualError(t, errs[0], `llamacpp.endpoints.embeddings.parameters.ctx-size: schema.yaml type "string" conflicts with Go spine kind "int"`)
}
