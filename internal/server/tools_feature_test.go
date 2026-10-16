package server

import (
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsCapabilityStates(t *testing.T) {
	for _, provider := range []string{"vllm", "ollama", "llamacpp", "mlx", "absent"} {
		for _, state := range []string{"present", "available", "unsupported", "unknown", "", "malformed"} {
			t.Run(provider+"/"+state, func(t *testing.T) {
				m := &cache.CachedModel{Name: "tool-capable-vision", Provider: provider, Details: map[string]any{"features": map[string]string{"tools": state}}}
				caps := deriveCapabilities(m, fakeApps(t))
				assert.Equal(t, state == "present", caps.Tools)
				raw, err := json.Marshal(caps)
				require.NoError(t, err)
				var wire map[string]any
				require.NoError(t, json.Unmarshal(raw, &wire))
				assert.Equal(t, state == "present", wire["tools"])
			})
		}
	}
}

func TestToolsGroupUnknownReplica(t *testing.T) {
	present := OpenAIModelObject{Capabilities: &OpenAIModelCapabilities{Tools: true, Chat: true}}
	for _, unknown := range []OpenAIModelObject{{}, {Capabilities: &OpenAIModelCapabilities{Chat: true}}} {
		for _, reverse := range []bool{false, true} {
			replicas := []modelgroup.Replica{{Model: "present"}, {Model: "unknown"}, {Model: "missing"}}
			if reverse {
				replicas[0], replicas[2] = replicas[2], replicas[0]
			}
			got := describeGroup("route", 1, replicas, map[string]OpenAIModelObject{"present": present, "unknown": unknown}, "local")
			require.NotNil(t, got.Capabilities)
			assert.False(t, got.Capabilities.Tools)
			assert.True(t, got.Capabilities.Chat)
		}
	}
}

func TestGroupMissingReplicaPreservesKnownCapabilities(t *testing.T) {
	known := OpenAIModelCapabilities{Chat: true, Stream: true, Completions: true, JSONMode: true, Embeddings: true, Rerank: true, Tools: true, Vision: true, MaxContextTokens: 8192}
	original := known
	other := known
	other.Rerank = false
	for _, missingFirst := range []bool{false, true} {
		for _, catalogued := range []bool{false, true} {
			replicas := []modelgroup.Replica{{Model: "known"}, {Model: "other"}, {Model: "missing"}}
			if missingFirst {
				replicas[0], replicas[2] = replicas[2], replicas[0]
			}
			byID := map[string]OpenAIModelObject{
				"known": {Capabilities: &known, Endpoints: []string{"chat", "completions"}},
				"other": {Capabilities: &other, Endpoints: []string{"chat"}},
			}
			if catalogued {
				byID["missing"] = OpenAIModelObject{} // Catalog entry without capabilities.
			}
			got := describeGroup("group", 1, replicas, byID, "local")
			want := other
			want.Tools, want.Vision, want.MaxContextTokens = false, false, 0
			assert.Equal(t, &want, got.Capabilities)
			assert.Equal(t, []string{"chat"}, got.Endpoints)
			assert.Equal(t, original, known, "catalog capabilities remain unchanged")
		}
	}
}

func TestToolsCloudExternalEvidenceContract(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		m := &cache.CachedModel{Name: "tool-capable", Provider: "ollama", IsCloud: cloud}
		assert.False(t, deriveCapabilities(m, fakeApps(t)).Tools)
		m.Details = map[string]any{"features": map[string]string{"tools": "present"}}
		assert.True(t, deriveCapabilities(m, fakeApps(t)).Tools)
	}
}
