package autoroute

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/group"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoRoute_ReplicaName(t *testing.T) {
	assert.Equal(t, "ollama", replicaName("ollama", ""))
	assert.Equal(t, "ollama-node1", replicaName("ollama", "node1"))
	assert.Equal(t, "vllm-gpu-server", replicaName("vllm", "gpu-server"))
}

func TestAutoRoute_IsAutoGroup(t *testing.T) {
	auto := &group.ModelGroup{AutoManaged: true}
	user := &group.ModelGroup{Description: "My custom route"}
	// A user-authored description that happens to begin with "[auto] "
	// is NOT auto-managed — the predicate reads the typed field, not
	// the description string.
	userLooksAuto := &group.ModelGroup{Description: "[auto] still mine"}
	empty := &group.ModelGroup{}

	assert.True(t, IsAutoGroup(auto))
	assert.False(t, IsAutoGroup(user))
	assert.False(t, IsAutoGroup(userLooksAuto))
	assert.False(t, IsAutoGroup(empty))
	assert.False(t, IsAutoGroup(nil))
}

func TestAutoRoute_SyncAllFromCache_SetsAutoManaged(t *testing.T) {
	store := group.NewGroupStore()
	models := []*cache.CachedModel{
		{Name: "llama3", Provider: "ollama", Node: "node-a"},
		{Name: "llama3", Provider: "vllm", Node: "node-b"},
	}
	m := New(store, "route-", func() {}, func() []*cache.CachedModel { return models })
	m.SyncAllFromCache()

	g := store.Get("route-llama3")
	require.NotNil(t, g)
	assert.True(t, g.AutoManaged)
	assert.Equal(t, "", g.AutoOwner)
	assert.NotContains(t, g.Description, "[auto]", "generator no longer stamps the prefix")
}

func TestAutoRoute_BuildReplicasFromInstances(t *testing.T) {
	instances := []*cache.CachedModel{
		{Name: "llama3", Provider: "ollama", Node: "node1"},
		{Name: "llama3", Provider: "ollama", Node: "node2"},
		{Name: "llama3", Provider: "ollama", Node: "node1"}, // duplicate
	}

	reps := buildReplicasFromInstances(instances)
	assert.Len(t, reps, 2)
	assert.Equal(t, "ollama-node1", reps[0].Name)
	assert.Equal(t, "ollama-node2", reps[1].Name)
}

func TestAutoRoute_NilManager(t *testing.T) {
	var m *Manager
	m.SyncAfterPull()
	m.SyncAfterDelete("llama3")
}

func TestAutoRoute_NilGroupStore(t *testing.T) {
	m := &Manager{}
	m.SyncAfterPull()
	m.SyncAfterDelete("llama3")
}

func TestAutoRoute_SyncAfterDelete_UserGroup(t *testing.T) {
	store := group.NewGroupStore()

	err := store.Set("llama3", group.ModelGroup{
		Description: "My custom route",
		Strategy:    group.StrategyPriority,
		Replicas: []group.Replica{
			{Name: "local", Model: "llama3", App: "ollama"},
		},
	})
	require.NoError(t, err)

	m := &Manager{groupStore: store}
	m.SyncAfterDelete("llama3")

	group := store.Get("llama3")
	require.NotNil(t, group)
	assert.Equal(t, "My custom route", group.Description)
}

func TestAutoRoute_RouteName(t *testing.T) {
	m := &Manager{routePrefix: "route-"}
	assert.Equal(t, "route-llama3", m.routeName("llama3"))
	assert.Equal(t, "route-qwen3-coder:latest", m.routeName("qwen3-coder:latest"))

	m2 := &Manager{routePrefix: ""}
	assert.Equal(t, "llama3", m2.routeName("llama3"))
}

func TestAutoRoute_PublishesAutoRouteGenerated(t *testing.T) {
	store := group.NewGroupStore()
	models := []*cache.CachedModel{
		{Name: "llama3", Provider: "ollama", Node: "node-a"},
		{Name: "llama3", Provider: "vllm", Node: "node-b"},
	}
	bus := route_events.NewBus()
	ch, _, unsub := bus.Subscribe(func(ev route_events.Event) bool {
		return ev.Type == route_events.EventAutoRouteGenerated
	})
	defer unsub()

	m := New(store, "route-", func() {}, func() []*cache.CachedModel { return models })
	m.SetEventBus(bus)
	m.SyncAllFromCache()

	select {
	case ev := <-ch:
		assert.Equal(t, "route-llama3", ev.Route)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("missing auto_route_generated event")
	}
}

func TestAutoRoute_PublishesAutoRouteRevoked(t *testing.T) {
	store := group.NewGroupStore()
	// Seed with an auto-managed group as if SyncAllFromCache had run.
	require.NoError(t, store.Set("route-llama3", group.ModelGroup{
		Description: "Route for llama3 across 2 replicas",
		AutoManaged: true,
		Replicas: []group.Replica{
			{Name: "llama3@ollama@node-a", Model: "llama3", App: "ollama", Node: "node-a"},
			{Name: "llama3@vllm@node-b", Model: "llama3", App: "vllm", Node: "node-b"},
		},
	}))
	// Cache now reports zero models, so the auto-route must be revoked.
	bus := route_events.NewBus()
	ch, _, unsub := bus.Subscribe(nil)
	defer unsub()

	// Return an empty (non-nil) slice so SyncAllFromCache walks the
	// cleanup branch — the nil short-circuit at the top is meant for
	// "cache not yet populated", not "cache emptied".
	m := New(store, "route-", func() {}, func() []*cache.CachedModel { return []*cache.CachedModel{} })
	m.SetEventBus(bus)
	m.SyncAllFromCache()

	deadline := time.After(300 * time.Millisecond)
	sawRevoked := false
	for {
		select {
		case ev := <-ch:
			if ev.Type == route_events.EventAutoRouteRevoked {
				assert.Equal(t, "route-llama3", ev.Route)
				sawRevoked = true
			}
		case <-deadline:
			assert.True(t, sawRevoked, "must see auto_route_revoked event after cache emptied")
			return
		}
	}
}

// TestAutoRoute_CleanupSpareseUserNamedGroup pins that the cleanup pass
// never deletes a group whose name this manager could not have generated.
//
// DELETE /:name/owner flips an arbitrary group to AutoManaged=true to hand
// it back to the generator. Before the prefix guard, the very next sync
// deleted such a group and persisted the deletion: activeRoutes only ever
// holds routePrefix+modelName, so a hand-authored name like "prod-chat" is
// unconditionally absent from it and read as "model gone".
func TestAutoRoute_CleanupSparesUserNamedGroup(t *testing.T) {
	store := group.NewGroupStore()
	// A hand-authored route that an operator just released ownership of.
	require.NoError(t, store.Set("prod-chat", group.ModelGroup{
		Description: "hand-authored production route",
		AutoManaged: true,
		Replicas: []group.Replica{
			{Name: "llama3@ollama@node-a", Model: "llama3", App: "ollama", Node: "node-a"},
		},
	}))
	// A real auto-route for a model that no longer exists — must still go.
	require.NoError(t, store.Set("route-gone", group.ModelGroup{
		AutoManaged: true,
		Replicas:    []group.Replica{{Name: "gone@ollama@node-a", Model: "gone", App: "ollama", Node: "node-a"}},
	}))

	m := New(store, "route-", func() {}, func() []*cache.CachedModel { return []*cache.CachedModel{} })
	m.SyncAllFromCache()

	assert.NotNil(t, store.Get("prod-chat"),
		"a user-named group must survive cleanup even when AutoManaged is set")
	assert.Nil(t, store.Get("route-gone"),
		"a generated auto-route whose model vanished must still be revoked")
}
