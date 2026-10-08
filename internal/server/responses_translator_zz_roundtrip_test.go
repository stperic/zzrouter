package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestZzNamespace_WireToTranslator_RoundTrip pins the streaming `zz_*`
// namespace contract between pkg/dispatch/wire (the injector) and
// internal/server/responses_translator (the extractor). Every field
// the wire writes into usage.zz_* must survive extractZzFromUsage with
// its `zz_` prefix stripped. The two ends are intentionally decoupled
// (the extractor is a bag-walk, not a hardcoded list) so adding a new
// metadata field on the wire side is a one-spot change. This test fails
// if the bag-walk is ever replaced with a hardcoded list that would
// silently drop new fields.
func TestZzNamespace_WireToTranslator_RoundTrip(t *testing.T) {
	meta := wire.RoutingMetadata{
		Provider:     "openrouter",
		Deployment:   "meta-llama/llama-3.2-3b-instruct",
		Node:         "macbook-pro",
		InjectUsage:  true,
		CostSource:   wire.CostSourceProvider,
		CostUSD:      0.0001234,
		LatencyMs:    250,
		TTFTMs:       120,
		TokensPerSec: 42.5,
	}

	// Build a synthetic terminal-usage SSE chunk in the OpenAI shape
	// the wire injector expects (completion_tokens > 0 marks it
	// terminal, gating the cost-stamp branch).
	chunk := []byte("data: " + `{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}` + "\n\n")

	injected := wire.InjectUsageIntoSSEChunk(chunk, meta)
	require.NotEqual(t, string(chunk), string(injected),
		"injector must rewrite the chunk")

	// Parse the usage block out of the injected chunk.
	usageRaw := extractUsageFromChunk(t, injected)

	// Run the translator's extractor over the usage block.
	zz := extractZzFromUsage(usageRaw)
	require.NotNil(t, zz, "extractor must find zz_* keys")

	// Required fields — present + values round-tripped exactly.
	assert.Equal(t, "openrouter", zz["provider"])
	assert.Equal(t, "macbook-pro", zz["node"])
	assert.Equal(t, meta.Deployment, zz["model"],
		"deployment field is renamed to model on the streaming side")
	assert.Equal(t, string(wire.CostSourceProvider), zz["cost_source"])
	assert.InDelta(t, 0.0001234, zz["cost_usd"], 1e-9)
	assert.EqualValues(t, 250, zz["latency_ms"])
	assert.EqualValues(t, 120, zz["ttft_ms"])
	assert.InDelta(t, 42.5, zz["tokens_per_second"], 1e-9)

	// No `zz_` prefixes remain after extraction (the translator strips
	// them on the way out).
	for k := range zz {
		assert.False(t, strings.HasPrefix(k, "zz_"),
			"extracted key %q must not retain the zz_ prefix", k)
	}

	// No extraneous keys leaked from canonical-usage fields
	// (prompt_tokens etc are not zz_-prefixed and must be filtered out).
	for _, banned := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		assert.NotContains(t, zz, banned,
			"non-zz_ usage field %q must not leak into the zzrouter block", banned)
	}
}

// TestZzNamespace_NonStreaming_NoZzPrefix pins the disjoint-namespaces
// rule: the non-streaming path uses a root `zzrouter` block with bare
// keys, never the `zz_` prefix that streaming uses inside usage. If
// these two ever cross-pollinate the wire becomes ambiguous.
func TestZzNamespace_NonStreaming_NoZzPrefix(t *testing.T) {
	meta := wire.RoutingMetadata{
		Provider:   "openrouter",
		Deployment: "x",
		Node:       "n",
		CostSource: wire.CostSourceProvider,
		CostUSD:    1.0,
		LatencyMs:  10,
	}
	body := wire.InjectRoutingMetadata([]byte(`{"id":"x","usage":{"prompt_tokens":1}}`), meta)

	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))

	zzAny, ok := out["zzrouter"].(map[string]any)
	require.True(t, ok, "non-streaming response must carry a root zzrouter block")
	for k := range zzAny {
		assert.False(t, strings.HasPrefix(k, "zz_"),
			"non-streaming root block must use bare keys, got %q", k)
	}
	// And the usage block must NOT have been touched with zz_*.
	usage, _ := out["usage"].(map[string]any)
	for k := range usage {
		assert.False(t, strings.HasPrefix(k, "zz_"),
			"non-streaming usage block must stay free of zz_* (leaked %q)", k)
	}
}

// extractUsageFromChunk pulls the `usage` JSON object out of the first
// `data: {...}` line in an SSE chunk.
func extractUsageFromChunk(t *testing.T, chunk []byte) json.RawMessage {
	t.Helper()
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data: ")) {
			continue
		}
		payload := trimmed[len("data: "):]
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &obj); err == nil && len(obj.Usage) > 0 {
			return obj.Usage
		}
	}
	t.Fatalf("no usage block found in chunk: %s", string(chunk))
	return nil
}
