package instance

import "github.com/stperic/zzrouter/pkg/constants"

// Status represents the lifecycle state of a provider instance.
type Status string

const (
	StatusStarting   Status = "starting"
	StatusRunning    Status = "running"
	StatusUnhealthy  Status = "unhealthy"
	StatusStopping   Status = "stopping"
	StatusStopped    Status = "stopped"
	StatusFailed     Status = "failed"
	StatusRestarting Status = "restarting"
)

// IsTerminal returns true if the status represents a final state (stopped or failed).
// Terminal instances should not block lifecycle operations like upgrade or uninstall.
func (s Status) IsTerminal() bool {
	return s == StatusStopped || s == StatusFailed
}

// LaunchMode represents how a provider process is launched.
type LaunchMode string

const (
	LaunchModeNative LaunchMode = "native"
)

// LaunchCommand is the structured command used to launch a process.
type LaunchCommand struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Environment map[string]string `json:"environment,omitempty"`
	WorkingDir  string            `json:"working_dir,omitempty"`
}

// Config contains configuration for launching an instance.
type Config struct {
	Runtime          string            `json:"runtime,omitempty"`
	DisposablePlanID string            `json:"disposable_plan_id,omitempty"`
	Provider         string            `json:"provider"`
	LaunchMode       LaunchMode        `json:"launch_mode"`
	Model            string            `json:"model,omitempty"`
	Port             int               `json:"port,omitempty"`
	EnvVars          map[string]string `json:"env_vars,omitempty"`
	Parameters       map[string]string `json:"parameters,omitempty"`
}

// InstanceInfo is a read-only snapshot of an instance for API responses.
// Public APIs return this instead of a mutable *Instance pointer.
type InstanceInfo struct {
	Runtime          string `json:"runtime,omitempty"`
	DisposablePlanID string `json:"disposable_plan_id,omitempty"`
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	LaunchMode       string `json:"launch_mode,omitempty"`
	Model            string `json:"model,omitempty"`
	// Endpoint is the per-request endpoint label this instance serves
	// ("chat", "embeddings", "reranking"). Empty defaults to chat.
	// Surfaced so cluster-wide consumers (e.g. /runs/ensure) can
	// distinguish multi-endpoint launches for one model.
	Endpoint              string            `json:"endpoint,omitempty"`
	SourceRepo            string            `json:"source_repo,omitempty"`
	SizeBytes             int64             `json:"size_bytes,omitempty"`
	Processor             string            `json:"processor,omitempty"`
	ContextLength         int               `json:"context_length,omitempty"`
	ProcessID             int               `json:"process_id,omitempty"`
	Port                  int               `json:"port"`
	Status                Status            `json:"status"`
	HealthURL             string            `json:"health_url"`
	Node                  string            `json:"node,omitempty"`
	StartedAt             string            `json:"started_at,omitempty"`
	Uptime                string            `json:"uptime,omitempty"`
	KeepAlive             string            `json:"keep_alive,omitempty"`
	LastActivity          string            `json:"last_activity,omitempty"`
	LastHealthCheck       string            `json:"last_health_check,omitempty"`
	ErrorMessage          string            `json:"error_message,omitempty"`
	Failure               *FailureInfo      `json:"failure,omitempty"`
	ActiveRequests        int64             `json:"active_requests,omitempty"`
	MaxConcurrentRequests int               `json:"max_concurrent_requests,omitempty"`
	Parameters            map[string]string `json:"parameters,omitempty"`
	// ResolvedParameters is what the process actually launched with,
	// as opposed to Parameters, which is only what the caller asked for.
	ResolvedParameters map[string]string `json:"resolved_parameters,omitempty"`
	Environment        map[string]string `json:"environment,omitempty"`
	// ParametersStatus compares what the run was launched with to what its
	// config resolves to now. Absent where nothing launched the run from
	// config (an external daemon's model) or the node predates the field;
	// either way it reads as unknown.
	ParametersStatus *ParametersStatus `json:"parameters_status,omitempty"`
}

// Resolved is what one launch computed from config: the values its
// process was given, before node-local paths replace asset names.
type Resolved struct {
	// AutoMemory records which parameter was resolved from auto at launch.
	AutoMemory string
	Runtime    string
	// Execution fingerprints the effective runtime and wire contract.
	Execution     string
	WireEndpoints []string
	Parameters    map[string]string
	Environment   map[string]string
	// Files maps each parameter that names a file to the digest of that
	// file's content, so a file replaced under the same name is a change.
	Files map[string]string
}

func (r Resolved) clone() Resolved {
	return Resolved{
		Runtime: r.Runtime, AutoMemory: r.AutoMemory,
		Execution: r.Execution, WireEndpoints: append([]string(nil), r.WireEndpoints...),
		Parameters:  copyStringMap(r.Parameters),
		Environment: copyStringMap(r.Environment),
		Files:       copyStringMap(r.Files),
	}
}

// ParametersState says whether a run still has what its config resolves to.
// The words match process.EnvState: only "stale" means a restart is owed.
type ParametersState string

const (
	ParametersCurrent ParametersState = "current"
	ParametersStale   ParametersState = "stale"
	// ParametersUnknown: the config as it stands would not launch, so
	// there is nothing to compare against, and a restart would leave the
	// model down. Error says why.
	ParametersUnknown ParametersState = "unknown"
)

// ParametersStatus is a run's ParametersState with the keys behind it, as
// "parameters.<key>" and "environment.<KEY>" paths.
type ParametersStatus struct {
	// Provider is the config key the run was judged against, which can
	// differ from the spelling the run was launched under ("llama.cpp").
	Provider string          `json:"provider"`
	State    ParametersState `json:"state"`
	// Changed lists what a restart would launch differently.
	Changed []string `json:"changed,omitempty"`
	// Overridden lists keys the run was launched with explicitly whose
	// config value now differs. A restart replays the run's own value, so
	// a config change to these does not reach it.
	Overridden []string `json:"overridden,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// FailedInstanceTTL is how long to keep failed instances before auto-cleanup.
const FailedInstanceTTL = constants.InstanceFailedTTL

// FileMount represents a file or directory to be made available to an instance.
type FileMount struct {
	Name      string `json:"name" yaml:"name"`                                 // Identifier (e.g., "model", "config")
	NodePath  string `json:"host_path" yaml:"host_path"`                       // Host path to the file or directory
	ReadOnly  bool   `json:"read_only,omitempty" yaml:"read_only,omitempty"`   // Whether mount is read-only
	IsOutput  bool   `json:"is_output,omitempty" yaml:"is_output,omitempty"`   // Whether this is an output file
	ArgFormat string `json:"arg_format,omitempty" yaml:"arg_format,omitempty"` // CLI arg format (e.g., "--model=%s")
	EnvVar    string `json:"env_var,omitempty" yaml:"env_var,omitempty"`       // Environment variable name for the path
}

// NativeConfig contains native process-specific configuration overrides.
type NativeConfig struct {
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}
