package cache

import (
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

func TestVariantConflict_ReservesNamesAcrossNodesAndAliases(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }})
	c.populateModelCacheFromItems([]map[string]any{{"name": "base", "node": "local"}})
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"), "a synthetic variant is not weights")

	// The local synthetic entry must not hide the remote physical name,
	// even when the physical canonical name has a repository prefix.
	c.populateModelCacheFromItems([]map[string]any{
		{"name": "repo/FAST", "source_id": "owner/weights", "node": "worker"},
		{"name": "base", "node": "local"},
	})
	for _, name := range []string{"fast", "FAST", "fast#Q4_K_M", "repo/FAST", "owner/weights", "OWNER/WEIGHTS"} {
		t.Run(name, func(t *testing.T) {
			err := c.CheckModelAdmission(t.Context(), name)
			require.ErrorIs(t, err, config.ErrModelNameConflict)
			var conflict *config.ModelNameConflictError
			require.ErrorAs(t, err, &conflict)
			assert.Equal(t, "worker", conflict.Node)
			node, found, err := c.WeightsNamed(t.Context(), name)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, "worker", node)
		})
	}
	require.NoError(t, c.CheckModelAdmission(t.Context(), "base"), "unrelated weights stay usable")
	models, err := c.ListModels(t.Context(), "", "", "", "")
	require.NoError(t, err)
	require.Len(t, models, 3, "the conflicted weights remain discoverable")

	c.populateModelCacheFromItems([]map[string]any{{"name": "base", "node": "local"}})
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"), "removing the conflicting weights restores admission")
}

func TestModelAdmission_IncompleteWeights(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "org/model"}})
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }})
	items := []map[string]any{{"name": "org/model", "node": "local", "assigned_app": "llamacpp", "incomplete": true}}
	c.populateModelCacheFromItems(items)
	models, err := c.ListModels(t.Context(), "", "", "", "")
	require.NoError(t, err)
	require.Len(t, models, 2)
	for _, m := range models {
		require.True(t, m.Incomplete)
		require.ErrorIs(t, c.CheckModelAdmission(t.Context(), m.Name), layout.ErrIncomplete)
		assert.False(t, c.HasModel(m.Name))
	}
	require.ErrorIs(t, c.CheckModelAdmission(t.Context(), "MODEL#Q4"), layout.ErrIncomplete)
	replicas, err := c.LookupReplicas(t.Context(), "org/model", "", "local", "")
	require.NoError(t, err)
	require.Empty(t, replicas)
	items = append(items, map[string]any{"name": "org/model", "node": "worker", "assigned_app": "llamacpp"})
	c.populateModelCacheFromItems(items)
	require.NoError(t, c.CheckModelAdmission(t.Context(), "org/model"))
	replicas, err = c.LookupReplicas(t.Context(), "org/model", "", "", "")
	require.NoError(t, err)
	require.Len(t, replicas, 1)
	assert.Equal(t, "worker", replicas[0].Node)
	items[0]["incomplete"] = false
	c.populateModelCacheFromItems(items)
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
	assert.True(t, c.HasModel("org/model"))
}

func TestVariantConflict_SourceAliasAndCloudExclusion(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }})
	c.updateModelCache([]*CachedModel{{Name: "base", Node: "local"}, {Name: "other", SourceID: "FAST", Node: "worker"}})
	require.ErrorIs(t, c.CheckModelAdmission(t.Context(), "other"), config.ErrModelNameConflict)
	require.ErrorIs(t, c.CheckModelAdmission(t.Context(), "fast"), config.ErrModelNameConflict)
	c.updateModelCache([]*CachedModel{{Name: "fast", IsCloud: true}})
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
}

// Invalidation can race admission. Refuse confirmed conflicts, but never
// populate the catalog from the admission hot path.
func TestVariantConflict_ConcurrentInvalidationUsesPublishedEvidence(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c := New(Config{Node: &fakeNode{}, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *config.AppsConfig { return cfg }})
	models := []*CachedModel{{Name: "fast", Node: "worker"}}
	c.updateModelCache(models)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.Invalidate()
			runtime.Gosched()
			c.updateModelCache(models)
			runtime.Gosched()
		}
	}()
	defer func() { close(stop); <-done }()
	for range 2000 {
		err := c.CheckModelAdmission(t.Context(), "fast")
		if err != nil {
			require.ErrorIs(t, err, config.ErrModelNameConflict)
		}
		_, found, err := c.WeightsNamed(t.Context(), "fast")
		if err == nil {
			require.True(t, found, "a missing snapshot must not report absent weights")
		}
	}
}

func TestVariantConflict_AdmissionNeverReadsCatalog(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }})
	c.updateModelCache([]*CachedModel{{Name: "FAST", SourceID: "owner/weights", Node: "worker"}})
	c.appsConfig = func() *config.AppsConfig { t.Fatal("admission read config"); return nil }
	c.registry = func() *modelregistry.Registry { t.Fatal("admission read registry"); return nil }
	c.node = &fakeNode{name: "worker"}
	require.ErrorIs(t, c.CheckModelAdmission(t.Context(), "owner/weights"), config.ErrModelNameConflict)
	require.NoError(t, c.CheckModelAdmission(t.Context(), "unrelated-cloud"))
	c.mu.Lock()
	c.invalidateLocked()
	c.mu.Unlock()
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
}

func TestVariantConflict_ConfigChangeRebuildsReservations(t *testing.T) {
	cfg := variantApps(t, true, nil)
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }, Registry: func() *modelregistry.Registry { return nil }})
	c.updateModelCache([]*CachedModel{{Name: "FAST", Node: "worker"}})
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
	fresh := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c.ReloadConfig(fresh)
	require.ErrorIs(t, c.CheckModelAdmission(t.Context(), "fast"), config.ErrModelNameConflict)
	c.ReloadConfig(cfg)
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
}

func TestVariantConflict_RejectsPublicationFromBeforeInvalidation(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }, Registry: func() *modelregistry.Registry { return nil }})
	generation := c.generation
	c.Invalidate()
	require.False(t, c.publishModelItems([]map[string]any{{"name": "FAST", "node": "worker"}}, generation, nil, localModelSnapshot{}, "", nil))
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
	assert.False(t, c.valid)
}

func TestVariantConflict_RequestChecksOnceAndFoldedNames(t *testing.T) {
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}, "k": {From: "base"}})
	c := New(Config{AppsConfig: func() *config.AppsConfig { return cfg }})
	c.updateModelCache([]*CachedModel{{Name: "FAST", Node: "worker"}, {Name: "K", Node: "worker"}})
	ctx := WithVariantAdmission(t.Context())
	require.ErrorIs(t, c.CheckModelAdmission(ctx, "fast"), config.ErrModelNameConflict)
	require.ErrorIs(t, c.CheckModelAdmission(ctx, "k"), config.ErrModelNameConflict)
	c.updateModelCache(nil)
	require.ErrorIs(t, c.CheckModelAdmission(ctx, "FAST#Q4"), config.ErrModelNameConflict, "same request retains its single admission result")
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"), "new request sees new evidence")
}

func TestVariantConflict_RefreshInvalidationOverlap(t *testing.T) {
	c := newWorkerCache(t)
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c.appsConfig = func() *config.AppsConfig { return cfg }
	c.updateModelCache([]*CachedModel{{Name: "fast", Node: "worker"}})
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var calls atomic.Int32
	c.registry = func() *modelregistry.Registry {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- c.RefreshCacheSync(t.Context()) }()
	<-entered
	c.Invalidate()
	close(release)
	require.NoError(t, <-done, "refresh must publish the generation after invalidation")
	assert.True(t, c.valid)
	assert.False(t, c.HasModel("fast"))
	require.NoError(t, c.CheckModelAdmission(t.Context(), "fast"))
}

func TestVariantConflict_EmptyCatalogIsValidAndUnavailableAdmissionContinues(t *testing.T) {
	c := newWorkerCache(t)
	c.updateModelCache(nil)
	models, err := c.ListModels(t.Context(), "", "", "", "")
	require.NoError(t, err)
	require.Empty(t, models)
	c.Invalidate()
	c.registry = func() *modelregistry.Registry { t.Fatal("unavailable admission attempted registry read"); return nil }
	require.NoError(t, c.CheckModelAdmission(t.Context(), "cloud-model"))
	require.True(t, c.admissionUnavailableLogged)
	require.NoError(t, c.CheckModelAdmission(t.Context(), "unrelated-model"))
}
