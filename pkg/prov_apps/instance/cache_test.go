package instance

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheSnapshotCannotMutateRun(t *testing.T) {
	run := NewInstance("r", "engine", "model", 0, 0, 0)
	data, err := json.Marshal(run.ToInfo("worker"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"cache"`)
	want := CacheCapacity{KVTokens: 32768, KVPools: 4, SequenceSlots: 4, ContextTokens: 32768, Sharing: "partitioned"}
	run.SetLoaded(Loaded{Cache: want, ContextLength: 8192})
	first, second := run.ToInfo("worker"), run.ToInfo("worker")
	require.NotNil(t, first.Cache)
	require.NotSame(t, first.Cache, second.Cache)
	first.Cache.KVTokens = 1
	loaded := run.Loaded()
	loaded.Cache.KVPools = 1
	assert.Equal(t, want, run.Loaded().Cache)
	assert.Equal(t, &want, second.Cache)
	data, err = json.Marshal(second)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	assert.Equal(t, float64(8192), wire["context_length"])
	assert.Equal(t, map[string]any{"kv_tokens": float64(32768), "kv_pools": float64(4), "sequence_slots": float64(4), "context_tokens": float64(32768), "sharing": "partitioned"}, wire["cache"])
}
