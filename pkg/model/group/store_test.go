package group

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validModelGroupsYAML = `
version: "1"

model_groups:
  fast-chat:
    description: "Fast chat with free-tier priority"
    strategy: priority
    replicas:
      - name: groq-free
        model: llama-3.3-70b-versatile
        provider: groq
        priority: 100
        timeout: 15s
      - name: openrouter-free
        model: meta-llama/llama-3.3-70b-instruct
        provider: openrouter
        priority: 50
        timeout: 15s
      - name: local-vllm
        model: llama-3.3-70b
        provider: vllm
        priority: 10
        timeout: 120s
        on_demand: true
        gpu_memory_required: 40GB

  code-gen:
    description: "Code generation"
    replicas:
      - name: local-vllm
        model: qwen2.5-coder-32b
        provider: vllm
        priority: 100
        on_demand: true
`

func TestGroupStore_LoadFromBytes(t *testing.T) {
	store := NewGroupStore()
	err := store.LoadFromBytes([]byte(validModelGroupsYAML))
	require.NoError(t, err)

	assert.Equal(t, 2, store.Len())
}

func TestGroupStore_Get(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	group := store.Get("fast-chat")
	require.NotNil(t, group)
	assert.Equal(t, "Fast chat with free-tier priority", group.Description)
	assert.Equal(t, StrategyPriority, group.Strategy)
	assert.Len(t, group.Replicas, 3)
}

func TestGroupStore_GetNotFound(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	assert.Nil(t, store.Get("nonexistent"))
}

func TestGroupStore_PriorityOrdering(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	group := store.Get("fast-chat")
	require.NotNil(t, group)
	require.Len(t, group.Replicas, 3)

	// Should be sorted by priority ascending (lower number = higher priority)
	assert.Equal(t, "local-vllm", group.Replicas[0].Name)
	assert.Equal(t, 10, group.Replicas[0].Priority)
	assert.Equal(t, "openrouter-free", group.Replicas[1].Name)
	assert.Equal(t, 50, group.Replicas[1].Priority)
	assert.Equal(t, "groq-free", group.Replicas[2].Name)
	assert.Equal(t, 100, group.Replicas[2].Priority)
}

func TestGroupStore_ReplicaFields(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	group := store.Get("fast-chat")
	require.NotNil(t, group)

	local := group.Replicas[0] // priority 10 = first
	assert.Equal(t, "llama-3.3-70b", local.Model)
	assert.Equal(t, "vllm", local.App)
	assert.Equal(t, 120*time.Second, local.Timeout.Duration)
	assert.True(t, local.OnDemand)
	assert.Equal(t, "40GB", local.GPUMemoryRequired)

	groq := group.Replicas[2] // priority 100 = last
	assert.Equal(t, "llama-3.3-70b-versatile", groq.Model)
	assert.Equal(t, "groq", groq.App)
	assert.Equal(t, 15*time.Second, groq.Timeout.Duration)
	assert.False(t, groq.OnDemand)
}

func TestGroupStore_DefaultStrategy(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	// code-gen has no explicit strategy, should default to priority
	group := store.Get("code-gen")
	require.NotNil(t, group)
	assert.Equal(t, StrategyPriority, group.Strategy)
}

func TestGroupStore_List(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	groups := store.List()
	assert.Len(t, groups, 2)
	assert.Contains(t, groups, "fast-chat")
	assert.Contains(t, groups, "code-gen")
}

func TestGroupStore_Names(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	names := store.Names()
	assert.Equal(t, []string{"code-gen", "fast-chat"}, names)
}

func TestGroupStore_LoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_groups.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validModelGroupsYAML), 0644))

	store := NewGroupStore()
	require.NoError(t, store.LoadFromFile(path))
	assert.Equal(t, 2, store.Len())
}

func TestGroupStore_LoadFromFile_NotFound(t *testing.T) {
	store := NewGroupStore()
	err := store.LoadFromFile("/nonexistent/model_groups.yaml")
	assert.Error(t, err)
}

func TestGroupStore_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing version",
			yaml:    `model_groups: {}`,
			wantErr: "missing required 'version'",
		},
		{
			name: "replica missing name",
			yaml: `
version: "1"
model_groups:
  test:
    replicas:
      - model: llama
        provider: vllm
`,
			wantErr: `replica 0 has no name`,
		},
		{
			name: "replica missing model",
			yaml: `
version: "1"
model_groups:
  test:
    replicas:
      - name: dep1
        provider: vllm
`,
			wantErr: `replica "dep1" has no model`,
		},
		{
			name: "replica missing app",
			yaml: `
version: "1"
model_groups:
  test:
    replicas:
      - name: dep1
        model: llama
`,
			wantErr: `replica "dep1" has no app`,
		},
		{
			name: "duplicate replica name",
			yaml: `
version: "1"
model_groups:
  test:
    replicas:
      - name: dep1
        model: llama
        provider: vllm
      - name: dep1
        model: mistral
        provider: vllm
`,
			wantErr: `duplicate replica name "dep1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewGroupStore()
			err := store.LoadFromBytes([]byte(tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestGroupStore_InvalidYAML(t *testing.T) {
	store := NewGroupStore()
	err := store.LoadFromBytes([]byte(`{invalid yaml`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse model groups config")
}

func TestGroupStore_Reload(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	assert.Equal(t, 2, store.Len())

	// Reload with different data
	newYAML := `
version: "1"
model_groups:
  only-one:
    replicas:
      - name: dep1
        model: llama
        provider: vllm
        priority: 1
`
	require.NoError(t, store.LoadFromBytes([]byte(newYAML)))
	assert.Equal(t, 1, store.Len())
	assert.Nil(t, store.Get("fast-chat"))
	assert.NotNil(t, store.Get("only-one"))
}

func TestGroupStore_EmptyGroups(t *testing.T) {
	yaml := `
version: "1"
model_groups: {}
`
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(yaml)))
	assert.Equal(t, 0, store.Len())
}

func TestGroupStore_NoModelGroupsSection(t *testing.T) {
	yaml := `
version: "1"
`
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(yaml)))
	assert.Equal(t, 0, store.Len())
}

func TestApplyPatch_NotFound(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	err := store.ApplyPatch("nonexistent", func(g ModelGroup) (ModelGroup, error) {
		return g, nil
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGroupNotFound))
}

func TestApplyPatch_MutateErrorAborts(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	before := store.Get("fast-chat")
	sentinel := errors.New("nope")
	err := store.ApplyPatch("fast-chat", func(g ModelGroup) (ModelGroup, error) {
		g.Description = "should not persist"
		return g, sentinel
	})
	require.ErrorIs(t, err, sentinel)
	after := store.Get("fast-chat")
	assert.Equal(t, before.Description, after.Description, "aborted patch must not mutate store")
}

func TestApplyPatch_ValidatesAfterMutate(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	err := store.ApplyPatch("fast-chat", func(g ModelGroup) (ModelGroup, error) {
		g.Strategy = "bogus"
		return g, nil
	})
	require.Error(t, err)
	assert.Equal(t, StrategyPriority, store.Get("fast-chat").Strategy, "validation failure must leave the store untouched")
}

func TestApplyPatch_LockSerializesConcurrentMutators(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))

	const writers = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(n int) {
			defer wg.Done()
			_ = store.ApplyPatch("fast-chat", func(g ModelGroup) (ModelGroup, error) {
				g.Description = fmt.Sprintf("writer-%d", n)
				return g, nil
			})
		}(i)
	}
	wg.Wait()

	got := store.Get("fast-chat")
	assert.NotNil(t, got)
	assert.Len(t, got.Replicas, 3, "concurrent patches must not corrupt replica list")
}

// Replica-level mutation goes through ApplyPatch closures. These tests
// exercise the sentinel propagation that handlers rely on.

func TestApplyPatch_ReplicaNotFoundSentinel(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	err := store.ApplyPatch("fast-chat", func(g ModelGroup) (ModelGroup, error) {
		for i := range g.Replicas {
			if g.Replicas[i].Name == "nope" {
				return g, nil
			}
		}
		return g, fmt.Errorf("%w: %q", ErrReplicaNotFound, "nope")
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReplicaNotFound))
}

func TestApplyPatch_ReplicaLastInGroupSentinel(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	err := store.ApplyPatch("code-gen", func(g ModelGroup) (ModelGroup, error) {
		if len(g.Replicas) <= 1 {
			return g, fmt.Errorf("%w: %q", ErrReplicaLastInGroup, g.Replicas[0].Name)
		}
		return g, nil
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReplicaLastInGroup))
	assert.Len(t, store.Get("code-gen").Replicas, 1, "store must be unchanged on refusal")
}

func TestStore_PublishesRouteCreatedOnNewSet(t *testing.T) {
	store := NewGroupStore()
	bus := route_events.NewBus()
	store.SetEventBus(bus)
	ch, _, unsub := bus.Subscribe(nil)
	defer unsub()

	require.NoError(t, store.Set("new-route", ModelGroup{
		Replicas: []Replica{{Name: "r1", Model: "m", App: "ollama"}},
	}))

	select {
	case ev := <-ch:
		assert.Equal(t, route_events.EventRouteCreated, ev.Type)
		assert.Equal(t, "new-route", ev.Route)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("missing route_created event")
	}
}

func TestStore_PublishesRouteUpdatedOnSecondSet(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.Set("r", ModelGroup{Replicas: []Replica{{Name: "x", Model: "m", App: "ollama"}}}))
	bus := route_events.NewBus()
	store.SetEventBus(bus)
	ch, _, unsub := bus.Subscribe(nil)
	defer unsub()

	require.NoError(t, store.Set("r", ModelGroup{Replicas: []Replica{{Name: "x", Model: "m2", App: "ollama"}}}))

	select {
	case ev := <-ch:
		assert.Equal(t, route_events.EventRouteUpdated, ev.Type,
			"second Set on the same name must emit updated, not created")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("missing route_updated event")
	}
}

func TestStore_PublishesRouteDeleted(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.Set("r", ModelGroup{Replicas: []Replica{{Name: "x", Model: "m", App: "ollama"}}}))
	bus := route_events.NewBus()
	store.SetEventBus(bus)
	ch, _, unsub := bus.Subscribe(nil)
	defer unsub()

	assert.True(t, store.Delete("r"))
	select {
	case ev := <-ch:
		assert.Equal(t, route_events.EventRouteDeleted, ev.Type)
		assert.Equal(t, "r", ev.Route)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("missing route_deleted event")
	}
}

func TestStore_ApplyPatch_PublishesReplicaDiff(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.Set("r", ModelGroup{Replicas: []Replica{
		{Name: "a", Model: "m", App: "ollama"},
		{Name: "b", Model: "m", App: "vllm"},
	}}))
	bus := route_events.NewBus()
	store.SetEventBus(bus)
	ch, _, unsub := bus.Subscribe(nil)
	defer unsub()

	require.NoError(t, store.ApplyPatch("r", func(g ModelGroup) (ModelGroup, error) {
		// replace b → c (one add + one remove)
		g.Replicas = []Replica{
			{Name: "a", Model: "m", App: "ollama"},
			{Name: "c", Model: "m", App: "ollama"},
		}
		return g, nil
	}))

	seen := make(map[route_events.EventType][]string)
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			seen[ev.Type] = append(seen[ev.Type], ev.Detail["replica"])
		case <-deadline:
			goto done
		}
	}
done:
	assert.Contains(t, seen, route_events.EventRouteUpdated)
	assert.Contains(t, seen[route_events.EventReplicaAdded], "c")
	assert.Contains(t, seen[route_events.EventReplicaRemoved], "b")
}

func TestStore_NoEventBusIsNoop(t *testing.T) {
	// No bus wired — Set/Delete/ApplyPatch must work without panicking.
	store := NewGroupStore()
	require.NoError(t, store.Set("r", ModelGroup{Replicas: []Replica{{Name: "x", Model: "m", App: "ollama"}}}))
	require.NoError(t, store.ApplyPatch("r", func(g ModelGroup) (ModelGroup, error) { return g, nil }))
	assert.True(t, store.Delete("r"))
}

func TestGroupStoreConcurrentSavesPersistLatestState(t *testing.T) {
	store := NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(validModelGroupsYAML)))
	path := filepath.Join(t.TempDir(), "model_groups.yaml")
	store.SetPath(path)
	const writers = 16
	start := make(chan struct{})
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := store.ApplyPatch("fast-chat", func(g ModelGroup) (ModelGroup, error) {
				g.Description = fmt.Sprintf("writer-%d", i)
				return g, nil
			})
			if err == nil {
				err = store.Save()
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	loaded := NewGroupStore()
	require.NoError(t, loaded.LoadFromFile(path))
	assert.Equal(t, store.List(), loaded.List(), "the final disk snapshot must include the latest mutation")
}
