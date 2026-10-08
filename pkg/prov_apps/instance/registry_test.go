package instance

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestInstance(id, provider, model string, port int) *Instance {
	return NewInstance(id, provider, model, port, 5*time.Minute, 10)
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)

	require.NoError(t, r.Register(inst))

	got, ok := r.Get("inst-1")
	require.True(t, ok)
	assert.Equal(t, inst, got)
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)

	require.NoError(t, r.Register(inst))
	err := r.Register(inst)
	assert.ErrorIs(t, err, ErrInstanceAlreadyExists)
}

func TestRegistry_RegisterDuplicateModel(t *testing.T) {
	r := NewRegistry()

	inst1 := newTestInstance("inst-1", "vllm", "llama3", 8000)
	inst1.MarkRunning()
	require.NoError(t, r.Register(inst1))

	inst2 := newTestInstance("inst-2", "vllm", "llama3", 8001)
	err := r.Register(inst2)
	assert.ErrorIs(t, err, ErrModelAlreadyLoaded)
}

func TestRegistry_RegisterReplacesFailedModel(t *testing.T) {
	r := NewRegistry()

	inst1 := newTestInstance("inst-1", "vllm", "llama3", 8000)
	inst1.MarkFailed("crash")
	require.NoError(t, r.Register(inst1))

	inst2 := newTestInstance("inst-2", "vllm", "llama3", 8001)
	require.NoError(t, r.Register(inst2))

	got, ok := r.GetByModel("llama3")
	require.True(t, ok)
	assert.Equal(t, "inst-2", got.ID)
}

func TestRegistry_GetByModel(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	require.NoError(t, r.Register(inst))

	got, ok := r.GetByModel("llama3")
	require.True(t, ok)
	assert.Equal(t, "inst-1", got.ID)

	_, ok = r.GetByModel("nonexistent")
	assert.False(t, ok)
}

func TestRegistry_GetByPort(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	require.NoError(t, r.Register(inst))

	got, ok := r.GetByPort(8000)
	require.True(t, ok)
	assert.Equal(t, "inst-1", got.ID)

	_, ok = r.GetByPort(9999)
	assert.False(t, ok)
}

func TestRegistry_Has(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	require.NoError(t, r.Register(inst))

	assert.True(t, r.Has("inst-1"))
	assert.False(t, r.Has("nonexistent"))
}

func TestRegistry_Remove(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	require.NoError(t, r.Register(inst))

	require.NoError(t, r.Remove("inst-1"))

	assert.False(t, r.Has("inst-1"))
	_, ok := r.GetByModel("llama3")
	assert.False(t, ok)
	_, ok = r.GetByPort(8000)
	assert.False(t, ok)
}

func TestRegistry_RemoveNotFound(t *testing.T) {
	r := NewRegistry()
	err := r.Remove("nonexistent")
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestRegistry_UpdateStatus(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	require.NoError(t, r.Register(inst))

	require.NoError(t, r.UpdateStatus("inst-1", StatusRunning, ""))
	assert.Equal(t, StatusRunning, inst.GetStatus())

	require.NoError(t, r.UpdateStatus("inst-1", StatusFailed, "oom"))
	assert.Equal(t, StatusFailed, inst.GetStatus())
	assert.Equal(t, "oom", inst.GetErrorMessage())

	err := r.UpdateStatus("nonexistent", StatusRunning, "")
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestRegistry_UpdateActivity(t *testing.T) {
	r := NewRegistry()
	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	require.NoError(t, r.Register(inst))

	require.NoError(t, r.UpdateActivity("inst-1", nil))
	assert.False(t, inst.GetLastActivity().IsZero())

	err := r.UpdateActivity("nonexistent", nil)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()

	inst1 := newTestInstance("inst-1", "vllm", "llama3", 8000)
	inst2 := newTestInstance("inst-2", "ollama", "mistral", 8001)
	require.NoError(t, r.Register(inst1))
	require.NoError(t, r.Register(inst2))

	all := r.List()
	assert.Len(t, all, 2)
}

func TestRegistry_ListRunning(t *testing.T) {
	r := NewRegistry()

	inst1 := newTestInstance("inst-1", "vllm", "llama3", 8000)
	inst1.MarkRunning()
	inst2 := newTestInstance("inst-2", "ollama", "mistral", 8001)
	// inst2 stays in Starting

	require.NoError(t, r.Register(inst1))
	require.NoError(t, r.Register(inst2))

	running := r.ListRunning()
	assert.Len(t, running, 1)
	assert.Equal(t, "inst-1", running[0].ID)
}

func TestRegistry_ListByProvider(t *testing.T) {
	r := NewRegistry()

	inst1 := newTestInstance("inst-1", "vllm", "llama3", 8000)
	inst2 := newTestInstance("inst-2", "vllm", "mistral", 8001)
	inst3 := newTestInstance("inst-3", "ollama", "phi", 8002)
	require.NoError(t, r.Register(inst1))
	require.NoError(t, r.Register(inst2))
	require.NoError(t, r.Register(inst3))

	vllm := r.ListByProvider("vllm")
	assert.Len(t, vllm, 2)

	ollama := r.ListByProvider("ollama")
	assert.Len(t, ollama, 1)
}

func TestRegistry_CountByProvider(t *testing.T) {
	r := NewRegistry()

	require.NoError(t, r.Register(newTestInstance("1", "vllm", "a", 8000)))
	require.NoError(t, r.Register(newTestInstance("2", "vllm", "b", 8001)))
	require.NoError(t, r.Register(newTestInstance("3", "ollama", "c", 8002)))

	assert.Equal(t, 2, r.CountByProvider("vllm"))
	assert.Equal(t, 1, r.CountByProvider("ollama"))
	assert.Equal(t, 0, r.CountByProvider("mlx"))
}

func TestRegistry_CleanupExpiredFailures(t *testing.T) {
	r := NewRegistry()

	inst := newTestInstance("inst-1", "vllm", "llama3", 8000)
	inst.MarkFailed("test")
	inst.FailedAt = time.Now().Add(-2 * FailedInstanceTTL) // expired
	require.NoError(t, r.Register(inst))

	inst2 := newTestInstance("inst-2", "vllm", "mistral", 8001)
	inst2.MarkRunning()
	require.NoError(t, r.Register(inst2))

	removed := r.CleanupExpiredFailures()
	assert.Equal(t, 1, removed)
	assert.False(t, r.Has("inst-1"))
	assert.True(t, r.Has("inst-2"))
}

func TestRegistry_Stats(t *testing.T) {
	r := NewRegistry()

	inst1 := newTestInstance("1", "vllm", "a", 8000)
	inst1.MarkRunning()
	inst2 := newTestInstance("2", "vllm", "b", 8001)
	inst2.MarkFailed("err")

	require.NoError(t, r.Register(inst1))
	require.NoError(t, r.Register(inst2))

	stats := r.Stats()
	counts, ok := stats["instances"].(map[string]int)
	require.True(t, ok)
	assert.Equal(t, 2, counts["total"])
	assert.Equal(t, 1, counts["running"])
	assert.Equal(t, 1, counts["failed"])
}

func TestRegistry_Concurrent(t *testing.T) {
	r := NewRegistry()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("inst-%d", n)
			model := fmt.Sprintf("model-%d", n)
			inst := newTestInstance(id, "vllm", model, 8000+n)

			if err := r.Register(inst); err != nil {
				return
			}

			r.Get(id)
			r.GetByModel(model)
			r.GetByPort(8000 + n)
			r.Has(id)
			_ = r.UpdateStatus(id, StatusRunning, "")
			_ = r.UpdateActivity(id, nil)
			r.List()
			r.ListRunning()
			r.ListByProvider("vllm")
			r.CountByProvider("vllm")
			r.Stats()
		}(i)
	}
	wg.Wait()
}

func TestGenerateID(t *testing.T) {
	id1 := GenerateID("vllm", "llama3", "chat")
	id2 := GenerateID("vllm", "llama3", "chat")

	assert.Len(t, id1, 12)
	assert.Len(t, id2, 12)
	assert.NotEqual(t, id1, id2) // includes nanosecond timestamp
}

// TestRegistry_PerEndpointInstancesCoexist is the load-bearing claim
// for endpoint-aware launch: registering (model, "chat") and
// (model, "embeddings") must both succeed and be independently
// retrievable. GetByModel(model) returns the chat instance.
func TestRegistry_PerEndpointInstancesCoexist(t *testing.T) {
	r := NewRegistry()

	chat := newTestInstance("inst-chat", "llamacpp", "qwen-0.5b", 8080)
	chat.Endpoint = "chat"
	chat.MarkRunning()
	require.NoError(t, r.Register(chat))

	embed := newTestInstance("inst-embed", "llamacpp", "qwen-0.5b", 8081)
	embed.Endpoint = "embeddings"
	embed.MarkRunning()
	require.NoError(t, r.Register(embed))

	gotChat, ok := r.GetByModelEndpoint("qwen-0.5b", "chat")
	require.True(t, ok)
	assert.Equal(t, "inst-chat", gotChat.ID)

	gotEmbed, ok := r.GetByModelEndpoint("qwen-0.5b", "embeddings")
	require.True(t, ok)
	assert.Equal(t, "inst-embed", gotEmbed.ID)

	// GetByModel shim defaults to chat.
	gotShim, ok := r.GetByModel("qwen-0.5b")
	require.True(t, ok)
	assert.Equal(t, "inst-chat", gotShim.ID)
}

// TestRegistry_DuplicateModelEndpoint covers that the duplicate-check
// is per-endpoint: registering a second chat instance for the same
// model fails, but adding an embeddings instance is allowed.
func TestRegistry_DuplicateModelEndpoint(t *testing.T) {
	r := NewRegistry()

	chat1 := newTestInstance("inst-chat-1", "llamacpp", "m", 8080)
	chat1.Endpoint = "chat"
	chat1.MarkRunning()
	require.NoError(t, r.Register(chat1))

	chat2 := newTestInstance("inst-chat-2", "llamacpp", "m", 8081)
	chat2.Endpoint = "chat"
	err := r.Register(chat2)
	assert.ErrorIs(t, err, ErrModelAlreadyLoaded)

	embed := newTestInstance("inst-embed", "llamacpp", "m", 8082)
	embed.Endpoint = "embeddings"
	embed.MarkRunning()
	assert.NoError(t, r.Register(embed))
}

// TestRegistry_RemoveOnlyClearsItsEndpointMapping prevents a Remove
// from invalidating the OTHER endpoint's lookup for the same model.
func TestRegistry_RemoveOnlyClearsItsEndpointMapping(t *testing.T) {
	r := NewRegistry()

	chat := newTestInstance("inst-chat", "llamacpp", "m", 8080)
	chat.Endpoint = "chat"
	chat.MarkRunning()
	require.NoError(t, r.Register(chat))

	embed := newTestInstance("inst-embed", "llamacpp", "m", 8081)
	embed.Endpoint = "embeddings"
	embed.MarkRunning()
	require.NoError(t, r.Register(embed))

	require.NoError(t, r.Remove("inst-chat"))

	_, ok := r.GetByModelEndpoint("m", "chat")
	assert.False(t, ok)
	got, ok := r.GetByModelEndpoint("m", "embeddings")
	require.True(t, ok)
	assert.Equal(t, "inst-embed", got.ID)
}

// TestGenerateID_EndpointDifferentiates is the bug-magnet zzgo flagged:
// two near-simultaneous launches for the same (provider, model) but
// different endpoints must produce different IDs even if the seq+nanos
// happen to collide.
func TestGenerateID_EndpointDifferentiates(t *testing.T) {
	idChat := GenerateID("llamacpp", "m", "chat")
	idEmbed := GenerateID("llamacpp", "m", "embeddings")
	assert.NotEqual(t, idChat, idEmbed)
}

func TestSanitizeModelName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"llama3", "llama3"},
		{"meta/llama3:70b", "meta-llama3-70b"},
		{"My Model", "my-model"},
		{"a-very-long-model-name-that-exceeds-thirty-two-characters-total", "a-very-long-model-name-that-exce"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, sanitizeModelName(tt.input))
	}
}
