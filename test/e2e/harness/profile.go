package harness

// Profile builders. Composition lives in code (not YAML) so changes
// refactor cleanly and IDE-jump-to-def works. The site YAML overlay is
// only for operator-tweakable leaves (addresses, tags, enable flags).
//
// All builders return fresh *Config — callers may mutate the result
// without affecting future calls.

// Base is the minimum-viable harness configuration: in-process backend,
// one coordinator, no workers, no providers, replay cassettes. Every
// other profile starts here.
func Base() *Config {
	return &Config{
		Backend: BackendInproc,
		Cluster: ClusterTopology{
			Coordinator: NodeSpec{
				Name:    "coord",
				Address: "127.0.0.1",
				Role:    RoleCoordinator,
			},
		},
		Cassettes: CassettesPolicy{
			Mode: CloudReplay,
			Dir:  "test/e2e/fixtures/cassettes",
		},
		Cleanup: CleanupPolicy{
			PreserveModelCache:  true,
			KeepArtifactsOnFail: true,
		},
		Rings: RingsPolicy{
			Ring1Contract: RingPolicy{Enabled: true, Parallelism: 8},
		},
		ModelCache:  "~/.cache/zzrouter-e2e/models",
		RequiredEnv: []string{"ZZROUTER_ADMIN_API_KEY", "ZZROUTER_CLUSTER_NETWORK_KEY"},
	}
}

// Local is the laptop profile: in-process 2-node cluster, Ring 1 only,
// no SSH, no cloud. The default for `make test-e2e-ring1`.
func Local() *Config {
	c := Base()
	c.Cluster.Workers = []NodeSpec{
		{
			Name:    "worker-local",
			Address: "127.0.0.1",
			Role:    RoleWorker,
			Tags:    []string{"inproc"},
		},
	}
	return c
}

// Nightly is the SSH-against-real-hardware profile: Ring 1 + Ring 2 +
// SDK compat + populated. Cloud requests go through cassettes by default.
//
// Operator-specific knobs (worker address, SSH key path, remote config
// dir, remote binary path) are intentionally LEFT EMPTY here; the site
// overlay must supply them. A site that forgets to do so trips the
// SSH-backend preflight rather than silently using a stale path.
//
// Tool-use note: a 1.5B model emits well-formed tool calls inconsistently.
// Ring 2 tool tests are expected to flake until a 7B+ fixture is wired
// in (RoleTools fixture is intentionally aspirational right now).
func Nightly() *Config {
	c := Base()
	c.Backend = BackendSSH
	// Worker admin port is 127.0.0.1-only (af3e7214) — leaving APIPort
	// empty here is intentional. The harness reaches the worker via
	// (a) the cluster mTLS listener (9091) for inference traffic, and
	// (b) SSH-tunneled loopback for admin-keyed operations like
	// /cluster/pair. Setting APIPort here would invite direct dials
	// that fail with connection-refused from outside the host.
	c.Cluster.Workers = []NodeSpec{
		{
			Name: "worker-1",
			Role: RoleWorker,
			Tags: []string{"gpu", "cuda"},
		},
	}
	c.Providers = []ProviderSpec{
		{Name: "llamacpp", Primary: true, InstallOn: []string{"coord", "worker-1"}},
		{Name: "ollama", InstallOn: []string{"coord"}},
		{Name: "vllm", InstallOn: []string{"worker-1"}, RequiresTags: []string{"gpu", "cuda"}},
		{Name: "openrouter", Cloud: true, APIKeyEnv: "OPENROUTER_API_KEY"}, //nolint:gosec // Synthetic fixture or metadata identifier, not a credential.
	}
	c.Models = []ModelSpec{
		{
			ID: "qwen2.5-1.5b-instruct-gguf", Provider: "llamacpp", Role: RoleChat,
			Source: "huggingface://Qwen/Qwen2.5-1.5B-Instruct-GGUF",
			File:   "qwen2.5-1.5b-instruct-q4_k_m.gguf", SizeMB: 1100, Primary: true,
		},
		{
			ID: "qwen2.5-1.5b-tools", Provider: "llamacpp", Role: RoleTools,
			Source: "huggingface://Qwen/Qwen2.5-1.5B-Instruct-GGUF",
			File:   "qwen2.5-1.5b-instruct-q4_k_m.gguf", SizeMB: 1100, Primary: true,
		},
		{
			ID: "nomic-embed-text", Provider: "llamacpp", Role: RoleEmbeddings,
			Source: "huggingface://nomic-ai/nomic-embed-text-v1.5-GGUF",
			File:   "nomic-embed-text-v1.5.Q4_K_M.gguf", SizeMB: 90, Primary: true,
		},
		{
			ID: "bge-reranker-v2-m3", Provider: "llamacpp", Role: RoleRerank,
			Source: "huggingface://gpustack/bge-reranker-v2-m3-GGUF",
			File:   "bge-reranker-v2-m3-Q4_K_M.gguf", SizeMB: 438, Primary: true,
		},
		{
			ID: "openrouter/qwen-2.5-7b-instruct", Provider: "openrouter",
			Role: RoleChat, Source: "cloud", Primary: true,
		},
	}
	c.Rings.Ring2Inference = RingPolicy{Enabled: true, Parallelism: 1}
	c.Rings.SDKCompat = RingPolicy{Enabled: true, Parallelism: 1}
	c.Rings.Populated = RingPolicy{Enabled: true, Parallelism: 1}
	return c
}

// Full enables every ring including chaos + soak + Ring 3. Used by the
// weekly `make test-e2e-full` target. Same node + provider set as Nightly.
func Full() *Config {
	c := Nightly()
	c.Rings.Ring3Curated = RingPolicy{Enabled: true, Parallelism: 1}
	c.Rings.Upgrade = RingPolicy{Enabled: true, Parallelism: 1}
	c.Rings.Chaos = RingPolicy{Enabled: true, Parallelism: 1}
	c.Rings.Soak = RingPolicy{Enabled: true, Parallelism: 1}
	c.Rings.CLI = RingPolicy{Enabled: true, Parallelism: 1}
	return c
}
