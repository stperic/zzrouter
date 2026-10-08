package prov_apps

import (
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// Endpoint identifies the request shape an instance serves. Used as a
// dimension in the launch-pool key when a provider opts in via
// runtime.endpoint_aware (llama.cpp's --embeddings is mode-exclusive).
type Endpoint string

const (
	EndpointChat       Endpoint = "chat"
	EndpointEmbeddings Endpoint = "embeddings"
	EndpointReranking  Endpoint = "reranking"
)

// LaunchRequest contains the parameters needed to launch a provider instance.
type LaunchRequest struct {
	installSmoke     bool
	selectedRuntime  string
	Runtime          string
	DisposablePlanID string
	Provider         string
	Model            string
	Endpoint         Endpoint // empty defaults to EndpointChat
	Port             int      // 0 = auto-allocate
	Parameters       map[string]string
	EnvVars          map[string]string
	Files            []instance.FileMount
	NativeConfig     *instance.NativeConfig
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
