package resolver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefault_Resolve_Local(t *testing.T) {
	// Lookup returns empty host when model is local — Resolved.Node should be empty.
	lookup := func(modelName string, nodeHint ...string) (string, string) {
		return "", "ollama"
	}
	r := NewDefault(lookup)

	got, err := r.Resolve(context.Background(), "llama3", "")
	assert.NoError(t, err)
	assert.Equal(t, "llama3", got.OriginalName)
	assert.Equal(t, "llama3", got.ModelName)
	assert.Equal(t, "", got.Node, "local model should have empty Node")
	assert.Equal(t, "ollama", got.Provider)
	assert.False(t, got.IsRemote())
}

func TestDefault_Resolve_Remote(t *testing.T) {
	lookup := func(modelName string, nodeHint ...string) (string, string) {
		return "gpu-1", "vllm"
	}
	r := NewDefault(lookup)

	got, err := r.Resolve(context.Background(), "llama3-70b", "")
	assert.NoError(t, err)
	assert.Equal(t, "gpu-1", got.Node)
	assert.Equal(t, "vllm", got.Provider)
	assert.True(t, got.IsRemote())
}

func TestDefault_Resolve_NodeHintForwarded(t *testing.T) {
	// The Default resolver must pass nodeHint through to the lookup so
	// X-Node pinning works at the cache layer.
	var seenHint string
	lookup := func(modelName string, nodeHint ...string) (string, string) {
		if len(nodeHint) > 0 {
			seenHint = nodeHint[0]
		}
		return "gpu-2", "ollama"
	}
	r := NewDefault(lookup)

	_, err := r.Resolve(context.Background(), "llama3", "pinned-node")
	assert.NoError(t, err)
	assert.Equal(t, "pinned-node", seenHint)
}
