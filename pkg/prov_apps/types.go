package prov_apps

import (
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// Endpoint separates incompatible request shapes when provider configuration requires it.
type Endpoint string

const (
	EndpointChat       Endpoint = "chat"
	EndpointEmbeddings Endpoint = "embeddings"
	EndpointReranking  Endpoint = "reranking"
)

// EnsureRequest contains the parameters needed to launch a provider instance.
type EnsureRequest struct {
	RunID            string `json:"run_id,omitempty"`
	refusal          error
	requestedModel   string
	keyEndpoint      Endpoint // Shared engines keep one key without discarding the requested shape.
	selectedRuntime  string
	Detached         bool                   `json:"-"` // returns an empty result on a cold start accepted in the background
	Origin           Origin                 `json:"-"`
	Force            bool                   `json:"-"`
	Unload           bool                   `json:"-"` // a zero keep-alive hint: start nothing for a model with no run
	Runtime          string                 `json:"runtime,omitempty"`
	DisposablePlanID string                 `json:"-"`
	Provider         string                 `json:"provider"`
	Model            string                 `json:"model,omitempty"`
	Endpoint         Endpoint               `json:"endpoint,omitempty"` // empty defaults to EndpointChat
	Port             int                    `json:"-"`                  // 0 = auto-allocate
	Parameters       map[string]string      `json:"parameters,omitempty"`
	EnvVars          map[string]string      `json:"env_vars,omitempty"`
	Files            []instance.FileMount   `json:"-"`
	NativeConfig     *instance.NativeConfig `json:"-"`
}

// Key returns the identity shared by registry lookups and concurrent ensures.
func (r EnsureRequest) Key() instance.Key {
	endpoint := r.Endpoint
	if r.keyEndpoint != "" {
		endpoint = r.keyEndpoint
	}
	return instance.NewKey(r.Provider, r.Model, string(endpoint))
}

func (r EnsureRequest) want() Want {
	return Want{Key: r.Key(), Provider: r.Provider, Runtime: r.Runtime, Plan: r.DisposablePlanID, Force: r.Force, Unload: r.Unload, Origin: r.Origin}
}

// EnsureResult reports a reused or newly started run; no run means the provider serves the model without one.
type EnsureResult struct {
	Run     *instance.Instance
	Started bool
}

// EndpointOrDefault returns EndpointChat for empty input.
func EndpointOrDefault(e Endpoint) Endpoint {
	if e == "" {
		return EndpointChat
	}
	return e
}

// InstallRequest contains the parameters for installing a provider.
type InstallRequest struct {
	Provider string // Provider name
	Version  string // Version to install (empty = latest, unless RequireVersionPin is set)
	Force    bool   // Reinstall over an existing IsInstalled=true install (runs Uninstall first under the per-provider lock).
}

// ProviderStatus represents the current state of a provider on this node.
type ProviderStatus struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Managed   bool   `json:"managed"`
	Version   string `json:"version,omitempty"`
	// ManagedProcess reports whether the managed install is the thing
	// actually running: "running", "absent", or "unknown". Version cannot
	// answer this -- a daemon started outside zzRouter reports whatever
	// version it is, including the one we installed.
	//
	// Omitted entirely when the question does not apply: a cloud provider
	// has no process, and an installer that cannot name its binary has no
	// way to be asked. Absent field means "not applicable"; "unknown"
	// means "applicable, could not tell".
	ManagedProcess process.Presence        `json:"managed_process,omitempty"`
	Mode           string                  `json:"mode"` // on-demand, service, external, cloud
	Enabled        bool                    `json:"enabled"`
	Instances      []instance.InstanceInfo `json:"instances,omitempty"`
}
