package prov_apps

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

type endpointTransport struct{ a, b *fakeDaemon }

func TestPhysicalPathDeletionReservationContract(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	a, b, alias := filepath.Join(root, "a.gguf"), filepath.Join(root, "b.gguf"), filepath.Join(root, "alias.gguf")
	for _, file := range []string{a, b} {
		require.NoError(t, os.WriteFile(file, []byte("GGUF"), 0600))
	}
	require.NoError(t, os.Symlink(a, alias))
	held := instance.Weights{Model: "alias", Paths: (process.ResolvedModel{Path: alias}).WeightPaths()}
	require.Equal(t, []string{a}, held.Paths)
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(b, alias))
	require.True(t, sameWeights(instance.Weights{Model: "a", Paths: []string{a}}, held), "retargeted alias cannot release the original weights")
	require.True(t, sameWeights(instance.Weights{Model: "directory", Paths: []string{root}}, held))
	require.False(t, sameWeights(instance.Weights{Model: "b", Paths: []string{alias}}, held))
}

func (r endpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/TenantB/") {
		copy := req.Clone(req.Context())
		copy.URL.Path = strings.TrimPrefix(copy.URL.Path, "/TenantB")
		return r.b.RoundTrip(copy)
	}
	return r.a.RoundTrip(req)
}

func TestQueuedDeletionReservationContract(t *testing.T) {
	barrier := newBarrier()
	r := newContractRig(t, true, WithProviderSchema(memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	})), func(m *ProviderAppManager) {
		m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
			return []gpu.MemoryDevice{{UUID: "GPU-test", TotalMiB: 10000, FreeMiB: 6000}}, nil
		}
	}, WithModelSource(func(_ context.Context, name string) string {
		if name == "new-base" {
			barrier.hold()
		}
		return ""
	}))
	cfg := copyReservationConfig(t, r.m.AppsConfig())
	require.NoError(t, cfg.UpdateApp("generic", func(sc *config.ServiceConfig) error {
		sc.Models = map[string]config.ModelSpec{"alias": {From: "old-base"}}
		return nil
	}))
	r.m.ReloadConfig(cfg)
	start := func(name string) *instance.Instance {
		result := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: name})})[0]
		require.NoError(t, result.err)
		return result.result.Run
	}
	first := start("blocker")
	queued := start("alias")
	require.False(t, queued.Admitted())
	require.Equal(t, "old-base", queued.Weights().Model)
	cfg = copyReservationConfig(t, r.m.AppsConfig())
	require.NoError(t, cfg.UpdateApp("generic", func(sc *config.ServiceConfig) error {
		sc.Models["alias"] = config.ModelSpec{From: "new-base"}
		return nil
	}))
	r.m.ReloadConfig(cfg)
	r.ready(t)
	awaitRunning(t, first)
	barrier.reached(t)
	writer := make(chan error, 1)
	go func() {
		writer <- r.m.WithModelDeletion(t.Context(), func(ctx context.Context) error {
			return r.m.CheckModelDeletion(ctx, instance.Weights{Model: "old-base"})
		})
	}()
	waitParkedOn(t, ").WithModelDeletion(", 1, contractWait)
	barrier.open()
	require.NoError(t, <-writer)
	awaitRunning(t, queued)
	require.Equal(t, "new-base", queued.Weights().Model)
	require.Error(t, r.m.CheckModelDeletion(t.Context(), instance.Weights{Model: "new-base"}))
}

func TestServedDeletionReservationContract(t *testing.T) {
	t.Run("protocol base is the physical endpoint", func(t *testing.T) {
		r := newServedRig(t)
		cfg := copyReservationConfig(t, r.config.Load())
		require.NoError(t, cfg.UpdateApp("ollama", func(sc *config.ServiceConfig) error { sc.Runtime.Endpoint = servedBase + "/v1/"; return nil }))
		r.config.Store(cfg)
		r.m.ReloadConfig(cfg)
		r.daemon.set(func(d *fakeDaemon) { d.loaded[servedModel] = time.Time{} })
		r.m.reconcileServed(t.Context(), "ollama")
		r.m.reconcileServed(t.Context(), "ollama")
		runs := r.m.instances.List()
		require.Len(t, runs, 1, "a canonical daemon observation must be adopted")
		require.Equal(t, servedBase, runs[0].Weights().Backend)
		up, _, err := r.m.Upstream(runs[0])
		require.NoError(t, err)
		require.Equal(t, servedBase, up)
	})
	t.Run("retarget preserves existing physical reservation", func(t *testing.T) {
		a, b := newFakeDaemon(), newFakeDaemon()
		r := newServedRig(t, WithHTTPClient(&http.Client{Transport: endpointTransport{a, b}}))
		r.daemon = a
		run := r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel})
		before := run.Weights()
		cfg := copyReservationConfig(t, r.config.Load())
		require.NoError(t, cfg.UpdateApp("ollama", func(sc *config.ServiceConfig) error { sc.Runtime.Endpoint = servedBase + "/TenantB"; return nil }))
		r.config.Store(cfg)
		r.m.ReloadConfig(cfg)
		r.m.reconcileServed(t.Context(), "ollama")
		_, held := r.m.instances.Get(run.ID)
		require.True(t, held, "B's empty inventory cannot release A's weights")
		require.Equal(t, before.Backend, run.Weights().Backend)
		require.Error(t, r.m.CheckModelDeletion(t.Context(), before))
		_, _, err := r.m.Upstream(run)
		require.ErrorIs(t, err, ErrProviderUnavailable)
	})
	t.Run("adoption retains observation endpoint", func(t *testing.T) {
		a, b := newFakeDaemon(), newFakeDaemon()
		r := newServedRig(t, WithHTTPClient(&http.Client{Transport: endpointTransport{a, b}}))
		r.daemon = a
		a.set(func(d *fakeDaemon) { d.loaded[servedModel] = time.Time{} })
		r.m.reconcileServed(t.Context(), "ollama")
		barrier := newBarrier()
		a.set(func(d *fakeDaemon) { d.psHook = barrier.hold })
		done := make(chan struct{})
		go func() { r.m.reconcileServed(t.Context(), "ollama"); close(done) }()
		barrier.reached(t)
		cfg := copyReservationConfig(t, r.config.Load())
		require.NoError(t, cfg.UpdateApp("ollama", func(sc *config.ServiceConfig) error { sc.Runtime.Endpoint = servedBase + "/TenantB"; return nil }))
		r.config.Store(cfg)
		r.m.ReloadConfig(cfg)
		barrier.open()
		<-done
		require.Empty(t, r.m.instances.List(), "A's observation cannot create a B reservation")
	})
	t.Run("case-sensitive tenant is a different backend", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel})
		weights := run.Weights()
		weights.Backend = servedBase + "/TenantA"
		run.SetWeights(weights)
		cfg := copyReservationConfig(t, r.config.Load())
		require.NoError(t, cfg.UpdateApp("ollama", func(sc *config.ServiceConfig) error { sc.Runtime.Endpoint = servedBase + "/tenanta"; return nil }))
		r.config.Store(cfg)
		r.m.ReloadConfig(cfg)
		_, _, err := r.m.Upstream(run)
		require.ErrorIs(t, err, ErrProviderUnavailable)
		require.False(t, sameWeights(weights, instance.Weights{Model: weights.Model, Backend: servedBase + "/tenanta"}))
		require.True(t, backend.SameEndpoint("HTTPS://GW.EXAMPLE/TenantA/", "https://gw.example/TenantA"))
	})
}

func TestChangedVariantDeletionReservationContract(t *testing.T) {
	r := newContractRig(t, false)
	cfg := copyReservationConfig(t, r.m.AppsConfig())
	require.NoError(t, cfg.UpdateApp("generic", func(sc *config.ServiceConfig) error {
		sc.Models = map[string]config.ModelSpec{"alias": {From: "base"}}
		return nil
	}))
	r.m.ReloadConfig(cfg)
	result := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "alias"})})[0]
	require.NoError(t, result.err)
	awaitRunning(t, result.result.Run)
	before := result.result.Run.Weights()
	require.Equal(t, "base", before.Model)
	for _, change := range []string{"changed", "removed"} {
		cfg = copyReservationConfig(t, r.m.AppsConfig())
		require.NoError(t, cfg.UpdateApp("generic", func(sc *config.ServiceConfig) error {
			if change == "changed" {
				sc.Models["alias"] = config.ModelSpec{From: "other"}
			} else {
				delete(sc.Models, "alias")
			}
			return nil
		}))
		r.m.ReloadConfig(cfg)
		require.Equal(t, before, result.result.Run.Weights())
		require.Error(t, r.m.CheckModelDeletion(t.Context(), instance.Weights{Model: "base"}))
	}
}

func copyReservationConfig(t *testing.T, before *config.AppsConfig) *config.AppsConfig {
	t.Helper()
	after := &config.AppsConfig{}
	before.RangeApps(func(name string, svc config.ServiceConfig) bool {
		require.NoError(t, after.AddApp(name, svc))
		return true
	})
	return after
}
