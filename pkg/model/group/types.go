// Package modelgroup provides model group configuration and lookup for the gateway API.
// A model group maps a logical model name (e.g., "fast-chat") to one or more replicas
// across different providers, enabling priority-based routing and future fallback.
package group

import (
	"time"
)

// StrategyType defines the replica selection strategy for a model group.
type StrategyType string

const (
	// StrategyPriority selects the highest-priority available replica.
	StrategyPriority StrategyType = "priority"

	// StrategyLeastLoad selects the replica with the fewest in-flight requests.
	StrategyLeastLoad StrategyType = "least-load"

	// StrategyFastest selects the replica with the lowest observed latency.
	StrategyFastest StrategyType = "fastest"
)

// modelGroupsFile is the on-disk format for model_groups.yaml.
type modelGroupsFile struct {
	Version     string                `yaml:"version"`
	ModelGroups map[string]ModelGroup `yaml:"model_groups"`
}

// GroupReader provides read-only access to model groups.
// Consumed by AccessControl for group-aware model access intersection.
type GroupReader interface {
	Get(name string) *ModelGroup
	Names() []string
}

// ModelGroup defines a logical model name that maps to one or more replicas.
type ModelGroup struct {
	Description string             `yaml:"description,omitempty"`
	Strategy    StrategyType       `yaml:"strategy,omitempty"`
	HealthCheck *HealthCheckConfig `yaml:"health_check,omitempty"`
	Replicas    []Replica          `yaml:"replicas"`
	Params      map[string]any     `yaml:"params,omitempty" json:"params,omitempty"` // Extensible parameters

	// AutoManaged is true when this group was created by the autoroute
	// generator from the model cache. Server-managed — refused as a
	// PATCH key; flipped to false by the implicit-claim path when a
	// user mutates the group.
	AutoManaged bool `yaml:"auto_managed,omitempty"`

	// AutoOwner identifies the principal that claimed an auto-managed
	// group via mutation. Set by the implicit-claim path; empty while
	// AutoManaged is true.
	AutoOwner string `yaml:"auto_owner,omitempty"`

	// Owner is the user-supplied audit tag for ephemeral routes.
	// Distinct from AutoOwner: Owner is set on PUT/PATCH from the
	// body's `owner` field, never mutated by the server, and exists
	// for human/dashboard audit. AutoOwner is server-managed and
	// reflects who claimed the route from autoroute generation.
	Owner string `yaml:"owner,omitempty"`

	// ExpiresAt marks an ephemeral route. When non-zero the background
	// reaper deletes the group at the next tick after this instant.
	// User-set via ttl_seconds on PUT/PATCH; never set by the
	// autoroute generator. Zero value means perpetual.
	ExpiresAt time.Time `yaml:"expires_at,omitempty"`
}

// HealthCheckConfig defines background health probe settings for a model group.
type HealthCheckConfig struct {
	Path     string   `yaml:"path,omitempty"`     // Health check endpoint (default "/health")
	Interval Duration `yaml:"interval,omitempty"` // Probe interval (default "30s")
	Timeout  Duration `yaml:"timeout,omitempty"`  // Probe timeout (default "5s")
}

// Replica represents one backend that can serve a model within a group.
// One replica is a single (node, provider, model) landing spot — analogous to
// a Kubernetes ReplicaSet member or a Postgres read replica.
type Replica struct {
	// Name is a human-readable label for this replica (e.g., "groq-free").
	Name string `yaml:"name"`

	// Model is the model identifier as known to this provider.
	Model string `yaml:"model"`

	// App is the app key in provider config (e.g., "groq", "vllm", "ollama").
	App string `yaml:"provider"`

	// Node is the target node ("" = local, hostname = remote).
	Node string `yaml:"node,omitempty"`

	// Priority controls selection order (lower number = tried first: 1 = primary, 2 = fallback).
	Priority int `yaml:"priority,omitempty"`

	// Timeout is the per-replica request timeout.
	Timeout Duration `yaml:"timeout,omitempty"`

	// OnDemand means the local model should be started if not running.
	OnDemand bool `yaml:"on_demand,omitempty"`

	// GPUMemoryRequired is the GPU memory needed for on-demand models (informational).
	GPUMemoryRequired string `yaml:"gpu_memory_required,omitempty"`

	// MaxRetries is the number of retries on this replica before falling back (default 1).
	MaxRetries int `yaml:"max_retries,omitempty"`

	// Tags are labels for tag-based routing (e.g., ["high-vram", "quantized"]).
	Tags []string `yaml:"tags,omitempty"`

	// Params holds extensible key-value parameters for future features
	// without requiring API changes.
	Params map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
}

// Duration wraps time.Duration for YAML string parsing (e.g., "15s", "2m").
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses a duration string like "15s" or "2m30s".
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML serializes the duration as a string.
func (d Duration) MarshalYAML() (any, error) {
	if d.Duration == 0 {
		return "", nil
	}
	return d.Duration.String(), nil
}
