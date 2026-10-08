package cache

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// fakeNode is a minimal NodeInfo for tests.
type fakeNode struct {
	name string
}

func (f *fakeNode) Nodename() string { return f.name }

// newWorkerCache builds a worker-mode Cache with all lazy accessors
// returning nil. After the role-agnostic cache cleanup, Start still
// runs warmLocalCache on a worker, but every dependency is nil-guarded
// (registry nil → empty local list; clusterClient nil → empty peers;
// appsConfig nil → empty cloud) so the warm lands on an empty set.
func newWorkerCache(t *testing.T) *Cache {
	t.Helper()
	cfg := &pkgConfig.NodeConfig{}
	cfg.Cluster.Mode = pkgConfig.ClusterModeWorker
	return New(Config{
		Node:                      &fakeNode{name: "worker-1"},
		Registry:                  func() *modelregistry.Registry { return nil },
		AppsConfig:                func() *pkgConfig.AppsConfig { return nil },
		ClusterClient:             func() mesh.ClusterClient { return nil },
		NodeCache:                 NewNodeResourceCache(),
		NodeConfig:                cfg,
		ResolveEndpointToNodename: func(s string) string { return s },
	})
}

// samplesFor returns a deterministic set of CachedModels for state tests.
func samplesFor() []*CachedModel {
	return []*CachedModel{
		{Name: "llama3", Provider: "ollama", Node: "gpu-1", SourceRepo: "ollama"},
		{Name: "qwen/qwen2.5-7b", Provider: "vllm", Node: "gpu-2", SourceRepo: "huggingface"},
		{Name: "gpt-4o", Provider: "openai", Node: "coordinator-1", IsCloud: true, SourceRepo: "openai"},
	}
}
