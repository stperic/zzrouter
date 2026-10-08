package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
)

// Group entries used to carry nothing but an id and owned_by, so an agent
// selecting on capabilities.chat skipped every route the router wanted it
// to prefer.
func TestDescribeGroup(t *testing.T) {
	t.Parallel()

	chatLocal := OpenAIModelObject{
		ID:           "qwen2.5:0.5b@macbook-pro",
		Capabilities: &OpenAIModelCapabilities{Chat: true, Stream: true, Tools: true, MaxContextTokens: 32768},
		Endpoints:    []string{"chat", "responses"},
		ServedBy:     servedByLocal,
	}
	chatCloud := OpenAIModelObject{
		ID:           "z-ai/glm-5.2:free",
		Capabilities: &OpenAIModelCapabilities{Chat: true, Stream: true, MaxContextTokens: 8192},
		Endpoints:    []string{"chat"},
		ServedBy:     servedByCloud,
	}
	byID := map[string]OpenAIModelObject{
		chatLocal.ID: chatLocal,
		chatCloud.ID: chatCloud,
	}

	t.Run("single local replica mirrors it", func(t *testing.T) {
		t.Parallel()
		got := describeGroup("route-qwen", 1, []modelgroup.Replica{
			{Name: "r1", Model: "qwen2.5:0.5b", Node: "macbook-pro", App: "ollama"},
		}, byID, "macbook-pro")

		assert.Equal(t, "zzrouter-route", got.OwnedBy)
		require.NotNil(t, got.Capabilities)
		assert.True(t, got.Capabilities.Chat)
		assert.True(t, got.Capabilities.Tools)
		assert.Equal(t, []string{"chat", "responses"}, got.Endpoints)
		assert.Equal(t, servedByLocal, got.ServedBy)
		assert.Nil(t, got.Runtime, "a group has no single instance")
	})

	t.Run("mixed replicas intersect and disclose cloud", func(t *testing.T) {
		t.Parallel()
		got := describeGroup("route-mixed", 1, []modelgroup.Replica{
			{Name: "r1", Model: "qwen2.5:0.5b", Node: "macbook-pro", App: "ollama"},
			{Name: "r2", Model: "z-ai/glm-5.2:free", App: "openrouter"},
		}, byID, "macbook-pro")

		require.NotNil(t, got.Capabilities)
		assert.True(t, got.Capabilities.Chat, "both replicas chat")
		assert.False(t, got.Capabilities.Tools, "only one replica has tools")
		assert.Equal(t, []string{"chat"}, got.Endpoints, "responses is not on both")
		assert.Equal(t, 8192, got.Capabilities.MaxContextTokens, "smallest window both can keep")
		assert.Equal(t, servedByCloud, got.ServedBy, "a request may bill upstream")
	})

	// Replica.Node documents "" as local, but the catalog id for a local
	// model is name@<hostname>. A hand-authored model_groups.yaml that
	// omits node: therefore looked up a key that cannot exist, and the
	// group fell back to the bare entry this function exists to replace.
	t.Run("local replica with no node still resolves", func(t *testing.T) {
		t.Parallel()
		got := describeGroup("route-local", 1, []modelgroup.Replica{
			{Name: "r1", Model: "qwen2.5:0.5b", App: "ollama"},
		}, byID, "macbook-pro")

		require.NotNil(t, got.Capabilities, "a replica with an empty node is local, not missing")
		assert.True(t, got.Capabilities.Chat)
		assert.Equal(t, servedByLocal, got.ServedBy)
	})

	t.Run("replicas absent from the catalog degrade to the bare entry", func(t *testing.T) {
		t.Parallel()
		got := describeGroup("route-unknown", 1, []modelgroup.Replica{
			{Name: "r1", Model: "not-in-catalog", App: "ollama"},
		}, byID, "macbook-pro")

		assert.Equal(t, "route-unknown", got.ID)
		assert.Nil(t, got.Capabilities)
		assert.Empty(t, got.ServedBy)
	})
}
