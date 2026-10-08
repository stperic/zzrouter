package resolver

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingResolver captures the last Resolve call for assertions.
type recordingResolver struct {
	lastModel    string
	lastNodeHint string
	ret          *Resolved
}

func (r *recordingResolver) Resolve(_ context.Context, modelName, nodeHint string) (*Resolved, error) {
	r.lastModel = modelName
	r.lastNodeHint = nodeHint
	return r.ret, nil
}

func TestGroup_Resolve_NodeHintSkipsGroup(t *testing.T) {
	// Explicit node pin must bypass group lookup (X-Node semantics).
	store := group.NewGroupStore()
	require.NoError(t, store.Set("fast-chat", group.ModelGroup{
		Strategy: group.StrategyPriority,
		Replicas: []group.Replica{{Name: "a", Model: "llama3", App: "ollama", Node: "gpu-1"}},
	}))

	inner := &recordingResolver{ret: &Resolved{OriginalName: "fast-chat", ModelName: "fast-chat", Node: "override"}}
	r := NewGroup(store, inner)

	got, err := r.Resolve(context.Background(), "fast-chat", "override-node")
	assert.NoError(t, err)
	assert.Equal(t, "override", got.Node)
	assert.Equal(t, "fast-chat", inner.lastModel)
	assert.Equal(t, "override-node", inner.lastNodeHint, "nodeHint must be forwarded to inner resolver")
	assert.Empty(t, got.GroupName, "group bypass must not set GroupName")
}

func TestGroup_Resolve_GroupHit(t *testing.T) {
	// GroupStore sorts replicas by priority ascending (lower = higher
	// priority), so the priority-5 replica is index 0 and wins.
	store := group.NewGroupStore()
	require.NoError(t, store.Set("fast-chat", group.ModelGroup{
		Strategy: group.StrategyPriority,
		Replicas: []group.Replica{
			{Name: "a", Model: "llama3-70b", App: "vllm", Node: "gpu-1", Priority: 10},
			{Name: "b", Model: "llama3-8b", App: "ollama", Node: "gpu-2", Priority: 5},
		},
	}))

	// Inner resolver must not be called on group hit.
	inner := &recordingResolver{ret: nil}
	r := NewGroup(store, inner)

	got, err := r.Resolve(context.Background(), "fast-chat", "")
	assert.NoError(t, err)
	assert.Equal(t, "fast-chat", got.OriginalName)
	assert.Equal(t, "llama3-8b", got.ModelName, "highest-priority (lowest number) replica wins")
	assert.Equal(t, "ollama", got.Provider)
	assert.Equal(t, "gpu-2", got.Node)
	assert.Equal(t, "fast-chat", got.GroupName, "group hit must populate GroupName")
	assert.Equal(t, "priority", got.Strategy)
	assert.Len(t, got.Candidates, 2, "all replicas exposed for fallback")
	assert.Empty(t, inner.lastModel, "inner resolver must not be called on group hit")
}

func TestGroup_Resolve_Miss_DelegatesToFallback(t *testing.T) {
	store := group.NewGroupStore()
	// no group for "direct-model"

	inner := &recordingResolver{ret: &Resolved{OriginalName: "direct-model", ModelName: "direct-model", Provider: "ollama"}}
	r := NewGroup(store, inner)

	got, err := r.Resolve(context.Background(), "direct-model", "")
	assert.NoError(t, err)
	assert.Equal(t, "direct-model", inner.lastModel)
	assert.Equal(t, "", inner.lastNodeHint)
	assert.Equal(t, "ollama", got.Provider)
	assert.Empty(t, got.GroupName)
}
