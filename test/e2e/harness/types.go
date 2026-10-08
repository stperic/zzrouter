// Package harness is the reusable e2e test library for zzrouter. It has
// zero *testing.T in its public surface — every helper returns errors so
// the same code drives unit tests, ad-hoc scripts, the CLI sweep tool,
// and chaos drivers. The harness/assert sibling package provides the
// one-line *testing.T shims tests actually use.
//
// See docs/plan_e2e_harness.md for the design contract.
package harness

import (
	"fmt"
	"time"
)

// Backend selects how the harness provisions the cluster under test.
type Backend string

const (
	BackendInproc Backend = "inproc" // single Go process, two ports — Ring 1
	BackendDocker Backend = "docker" // docker-compose — local Ring 2
	BackendSSH    Backend = "ssh"    // remote hosts over SSH — nightly Ring 2+
)

// Role names a node's cluster role. Mirrors pkg/cluster/role; we don't
// import the production type to keep harness compilable without cluster
// state.
type Role string

const (
	RoleCoordinator Role = "coordinator"
	RoleWorker      Role = "worker"
)

// CloudMode controls how cloud-provider HTTP egress is recorded/replayed.
// Default is Replay so CI never makes a real cloud call.
type CloudMode string

const (
	CloudReplay CloudMode = "replay"
	CloudRecord CloudMode = "record"
	CloudLive   CloudMode = "live"
)

// ModelRole tags a fixture model's intended use. The PrimaryModel
// lookup picks one fixture per role.
type ModelRole string

const (
	RoleChat       ModelRole = "chat"
	RoleEmbeddings ModelRole = "embeddings"
	RoleRerank     ModelRole = "rerank"
	RoleVision     ModelRole = "vision"
	RoleTools      ModelRole = "tools"
)

// NodeSpec describes a single node in the cluster topology. Operators
// edit these via the site YAML overlay; engineers edit defaults via
// profile builders. Pointer-as-tristate Enabled — nil means "default
// enabled", explicit false in the overlay disables without removing.
type NodeSpec struct {
	Name    string   `yaml:"name"`
	Address string   `yaml:"address"`
	APIPort int      `yaml:"api_port"`
	Role    Role     `yaml:"role"`
	Tags    []string `yaml:"tags,omitempty"`
	Enabled *bool    `yaml:"enabled,omitempty"`

	// ClusterPort is the mTLS cluster-listener port (default 9091).
	// On the coordinator it's the URL workers reach during pairing
	// and as the runtime data path. On a worker it's the only
	// network-reachable surface (admin port is 127.0.0.1-only since
	// af3e7214). Zero-value resolves to 9091 in ClusterURL().
	ClusterPort int `yaml:"cluster_port,omitempty"`

	// CoordinatorURL is an optional per-worker override for the coord
	// URL the worker is told to dial during pairing. Multi-network
	// topologies (e.g. one worker on LAN, another over tailnet) need
	// different coord URLs even though coord is one process.
	//
	// When empty, PairWorker derives the URL from the coordinator
	// NodeSpec. When set, PairWorker uses this value verbatim — both
	// for fetchCAFingerprint and the /cluster/pair body's
	// coordinator_url. Format: "https://<host>:<port>".
	CoordinatorURL string `yaml:"coordinator_url,omitempty"`

	// SSH provisioning details — only consulted when Backend == BackendSSH.
	SSH SSHSpec `yaml:"ssh,omitempty"`

	// Remote paths used by the SSH backend. Operator-supplied via the
	// site overlay; the profile builders leave these empty so different
	// sites can layout zzrouter differently without a code change.
	ConfigDir string `yaml:"config_dir,omitempty"`
	Binary    string `yaml:"binary,omitempty"`
}

// IsEnabled reports whether the node is active. Absent flag = enabled.
func (n NodeSpec) IsEnabled() bool {
	if n.Enabled == nil {
		return true
	}
	return *n.Enabled
}

// ClusterURL returns the https://addr:port URL workers reach for the
// mTLS cluster listener. ClusterPort==0 resolves to 9091 (the
// platform default). Empty Address returns "" — callers should treat
// that as a config error.
func (n NodeSpec) ClusterURL() string {
	if n.Address == "" {
		return ""
	}
	port := n.ClusterPort
	if port == 0 {
		port = 9091
	}
	return fmt.Sprintf("https://%s:%d", n.Address, port)
}

// SSHSpec carries dial credentials. Secrets are NOT inlined — the
// harness reads keys from disk at provision time.
type SSHSpec struct {
	Host    string `yaml:"host,omitempty"`
	Port    int    `yaml:"port,omitempty"` // 0 → 22
	User    string `yaml:"user,omitempty"`
	KeyPath string `yaml:"key_path,omitempty"`
	// sudo is intentionally absent — see plan §2.
}

// ClusterTopology bundles the coordinator + workers.
type ClusterTopology struct {
	Coordinator NodeSpec   `yaml:"coordinator"`
	Workers     []NodeSpec `yaml:"workers,omitempty"`
}

// ProviderSpec declares a provider the harness should install + drive.
// install_on resolves to NodeSpec.Name; requires_tags filters at runtime
// (auto-skip rather than fail when a node lacks the tags).
type ProviderSpec struct {
	Name         string   `yaml:"name"`
	Primary      bool     `yaml:"primary,omitempty"` // ring2 picks primary
	Cloud        bool     `yaml:"cloud,omitempty"`   // routes through cassettes
	APIKeyEnv    string   `yaml:"api_key_env,omitempty"`
	InstallOn    []string `yaml:"install_on,omitempty"`
	RequiresTags []string `yaml:"requires_tags,omitempty"`
	Disabled     bool     `yaml:"disabled,omitempty"`
}

// ModelSpec is a single fixture model. Source schemes:
//   - huggingface://<repo>      (file optional — picks recommended GGUF)
//   - ollama://<model>:<tag>
//   - cloud                      (no local download; hits provider)
//
// Primary is per-(role, IsCloud) — one local primary and one cloud
// primary may coexist for the same role. Validate enforces that.
type ModelSpec struct {
	ID       string    `yaml:"id"`
	Provider string    `yaml:"provider"`
	Role     ModelRole `yaml:"role"`
	Source   string    `yaml:"source"`
	File     string    `yaml:"file,omitempty"`
	SizeMB   int       `yaml:"size_mb,omitempty"`
	SHA256   string    `yaml:"sha256,omitempty"`
	Primary  bool      `yaml:"primary,omitempty"`
}

// IsCloud reports whether this fixture targets a cloud provider (no
// local download required).
func (m ModelSpec) IsCloud() bool { return m.Source == "cloud" }

// Timeouts caps every long-running harness operation. Zero values mean
// "use the package default" (see config.go defaults table).
type Timeouts struct {
	InstallProvider time.Duration `yaml:"install_provider,omitempty"`
	DownloadModel   time.Duration `yaml:"download_model,omitempty"`
	LaunchRun       time.Duration `yaml:"launch_run,omitempty"`
	Inference       time.Duration `yaml:"inference,omitempty"`
	JobTerminal     time.Duration `yaml:"job_terminal,omitempty"`
	Preflight       time.Duration `yaml:"preflight,omitempty"`
}

// CleanupPolicy controls per-test isolation behavior.
type CleanupPolicy struct {
	SnapshotBeforeRun   bool `yaml:"snapshot_before_run,omitempty"`
	RestoreAfterRun     bool `yaml:"restore_after_run,omitempty"`
	PreserveModelCache  bool `yaml:"preserve_model_cache,omitempty"`
	KeepArtifactsOnFail bool `yaml:"keep_artifacts_on_failure,omitempty"`
}

// CassettesPolicy configures the cloud HTTP record/replay layer.
type CassettesPolicy struct {
	Mode CloudMode `yaml:"mode,omitempty"`
	Dir  string    `yaml:"dir,omitempty"`
}

// RingPolicy enables/disables a test ring + its parallelism.
type RingPolicy struct {
	Enabled     bool `yaml:"enabled,omitempty"`
	Parallelism int  `yaml:"parallelism,omitempty"`
}

// RingsPolicy aggregates all ring policies. Names match the test
// directory layout under test/e2e/.
type RingsPolicy struct {
	Ring1Contract  RingPolicy `yaml:"ring1,omitempty"`
	Ring2Inference RingPolicy `yaml:"ring2,omitempty"`
	Ring3Curated   RingPolicy `yaml:"ring3,omitempty"`
	Populated      RingPolicy `yaml:"populated,omitempty"`
	Upgrade        RingPolicy `yaml:"upgrade,omitempty"`
	Chaos          RingPolicy `yaml:"chaos,omitempty"`
	Soak           RingPolicy `yaml:"soak,omitempty"`
	SDKCompat      RingPolicy `yaml:"sdk_compat,omitempty"`
	CLI            RingPolicy `yaml:"cli,omitempty"`
}
