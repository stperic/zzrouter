package config

import (
	"fmt"
	"os"
)

// NodeConfig represents the complete host configuration
type NodeConfig struct {
	// Node configuration
	Node ServeConfig `mapstructure:"node" yaml:"node"`

	// Cluster networking
	Cluster ClusterConfig `mapstructure:"cluster" yaml:"cluster"`

	// Authentication settings
	Auth AuthConfig `mapstructure:"auth" yaml:"auth"`

	// Security settings
	Security SecurityConfig `mapstructure:"security" yaml:"security,omitempty"`

	// Discovery settings
	MDNSDiscovery bool            `mapstructure:"mdns_discovery" yaml:"mdns_discovery"`
	Discovery     DiscoveryConfig `mapstructure:"discovery" yaml:"discovery,omitempty"`

	// Model repository configuration
	Models ModelsConfig `mapstructure:"models" yaml:"models,omitempty"`

	// Cache configuration
	Cache CacheConfig `mapstructure:"cache" yaml:"cache,omitempty"`

	// Observability configuration (OpenTelemetry)
	Observability ObservabilityConfig `mapstructure:"observability" yaml:"observability,omitempty"`

	// Coordinator-only settings (routing, chat). Ignored on workers.
	Coordinator CoordinatorConfig `mapstructure:"coordinator" yaml:"coordinator,omitempty"`

	// Auto-update configuration
	Update UpdateConfig `mapstructure:"update" yaml:"update,omitempty"`

	// Inference log configuration (in-memory request logging)
	InferenceLog InferenceLogConfig `mapstructure:"inference_log" yaml:"inference_log,omitempty"`

	// OpenAI compatibility configuration (stateful /v1/* pass-through)
	OpenAICompat OpenAICompatConfig `mapstructure:"openai_compat" yaml:"openai_compat,omitempty"`

	// Native-wire passthrough configuration. Maps mount paths such as
	// /anthropic/* or /vertex_ai/* to provider keys registered in
	// provider config, so that clients can keep using vendor-native SDKs
	// against zzrouter without reshaping the wire format.
	NativeWire NativeWireConfig `mapstructure:"native_wire" yaml:"native_wire,omitempty"`

	// MCP transport gateway configuration. Mounts each configured MCP
	// server on /mcp/{name}/* and proxies the transport end-to-end.
	// Supports remote HTTP/SSE servers via URL and local stdio servers
	// via child-process bridging. zzrouter is a transport proxy only —
	// it does not implement tool registries, ACLs, or protocol
	// translation on top of MCP.
	MCP MCPConfig `mapstructure:"mcp" yaml:"mcp,omitempty"`

	// Providers controls knobs that affect how provider installs
	// (vllm, mlx, llamacpp, ollama) behave: install directory override,
	// corporate proxy / CA bundle for downloads, and whether to run the
	// venv read-only hardening step. Values flow into
	// pkg/prov_apps/install at server boot.
	Providers ProvidersConfig `mapstructure:"providers" yaml:"providers,omitempty"`

	// Backward compatibility: old top-level routing/chat still deserializable
	Routing RoutingConfig `mapstructure:"routing" yaml:"-"`
	Chat    ChatConfig    `mapstructure:"chat" yaml:"-"`

	// SourcePath is the file this config was read from. Not persisted:
	// it describes where the config came from, not what it says. A node
	// run as a service reads a different file than the operator's CLI
	// does, and without this there is nothing to compare.
	SourcePath string `mapstructure:"-" yaml:"-"`
}

// CoordinatorConfig holds settings that only apply when this node is a
// coordinator (or standalone). Workers silently ignore this section.
type CoordinatorConfig struct {
	// Routing configuration (resource-aware model routing)
	Routing RoutingConfig `mapstructure:"routing" yaml:"routing,omitempty"`

	// Chat configuration (slash commands in chat interfaces)
	Chat ChatConfig `mapstructure:"chat" yaml:"chat,omitempty"`
}

// OpenAICompatConfig holds settings for OpenAI-compatible API pass-through
// routing. These apply to endpoints under /v1/* that cannot be routed by a
// model field (Files, Batches, Assistants, Threads, Vector Stores, Uploads,
// Responses retrieval, and similar stateful resources).
type OpenAICompatConfig struct {
	// DefaultBackend is the provider key used to route stateful /v1/*
	// endpoints when no model field is available. Must match a registered
	// provider in provider config that runs in service or cloud mode (i.e.
	// exposes a persistent endpoint URL). When empty, stateful endpoints
	// return 503 no_default_backend_configured.
	//
	// State created by one backend is NOT synced to other cluster nodes.
	// Files uploaded to backend A are not visible on backend B.
	DefaultBackend string `mapstructure:"default_backend" yaml:"default_backend,omitempty"`

	// MaxUploadBytes caps the request body size for multipart uploads
	// (audio, images, files). Defaults to 25 MiB at request time when
	// unset or non-positive, matching OpenAI's own cap for audio inputs.
	MaxUploadBytes int64 `mapstructure:"max_upload_bytes" yaml:"max_upload_bytes,omitempty"`

	// RealtimeBackend is the provider key used for /v1/realtime WebSocket
	// upgrades and the /v1/realtime/sessions helper endpoints. The
	// provider must be registered in provider config with a runtime
	// endpoint that implements the OpenAI Realtime WebSocket protocol.
	// When empty, /v1/realtime returns 501.
	RealtimeBackend string `mapstructure:"realtime_backend" yaml:"realtime_backend,omitempty"`
}

// MCPConfig holds MCP (Model Context Protocol) transport gateway
// configuration. zzrouter exposes each configured server at /mcp/{name}/*
// and proxies the transport end-to-end.
//
// Example node.yaml:
//
//	mcp:
//	  servers:
//	    github:
//	      transport: http
//	      url: https://mcp-github.example.com/v1/mcp
//	    filesystem:
//	      transport: stdio
//	      command: npx
//	      args: ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
//	      env:
//	        LOG_LEVEL: debug
//
// With that config, an MCP client pointed at http://zzrouter:9090/mcp/github
// reaches the remote MCP server unchanged, and a client pointed at
// http://zzrouter:9090/mcp/filesystem is bridged to the locally spawned
// filesystem server via its stdio pipes.
//
// Scope: transport passthrough only. zzrouter does not implement a tool
// registry, per-key ACLs, OpenAPI→MCP conversion, or any orchestration
// above the wire protocol. All of that is intentionally left to the MCP
// client and the servers themselves.
type MCPConfig struct {
	// Servers maps the MCP mount name (used under /mcp/{name}/*) to
	// its transport configuration. The map keys must be safe URL path
	// segments — the registrar rejects anything containing a slash.
	Servers map[string]MCPServerConfig `mapstructure:"servers" yaml:"servers,omitempty"`
}

// MCPServerConfig describes a single MCP server mount. `Transport`
// selects which other fields are consumed:
//
//   - transport: http   — `url` is required.
//   - transport: sse    — same as http; kept as a separate value so that
//     documentation and startup logs can describe the server's
//     expected wire format accurately.
//   - transport: stdio  — `command` is required; `args` and `env` are
//     optional. The executable is spawned on the first request to the
//     mount and kept alive for the lifetime of the zzrouter process.
type MCPServerConfig struct {
	// Transport selects the wire transport: http, sse, or stdio.
	// Defaults to http when empty.
	Transport string `mapstructure:"transport" yaml:"transport,omitempty"`

	// URL is the full upstream endpoint for http and sse transports.
	// Ignored for stdio.
	URL string `mapstructure:"url" yaml:"url,omitempty"`

	// Command is the executable to spawn for stdio transport.
	// Required when transport=stdio, ignored otherwise.
	Command string `mapstructure:"command" yaml:"command,omitempty"`

	// Args are the command-line arguments passed to Command.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`

	// Env is additional environment overrides for the spawned stdio
	// process. Merged on top of zzrouter's own environment.
	Env map[string]string `mapstructure:"env" yaml:"env,omitempty"`

	// API is what an http or sse mount's server is sent to authenticate,
	// as a provider's runtime.api says it (bearer with a token, or caller
	// for the caller's own key). Absent, it is sent nothing of the
	// caller's: an MCP server is a third party, never a zzRouter key holder.
	API *APIConfig `mapstructure:"api" yaml:"api,omitempty"`
}

// NativeWireConfig holds native-wire passthrough mounts. Each mount exposes
// a vendor-native API surface (Anthropic Messages, Vertex AI, Bedrock, etc.)
// at a dedicated path prefix, forwarding requests to a configured provider
// backend without reshaping the wire format.
//
// Example node.yaml:
//
//	native_wire:
//	  mounts:
//	    anthropic: anthropic-prod
//	    vertex_ai: vertex-ai-prod
//	    bedrock:   bedrock-prod
//
// With that config, a client SDK pointed at http://zzrouter:9090/anthropic
// reaches the provider registered as "anthropic-prod" in provider config.
// zzrouter auth is stripped and provider auth is injected for cloud-mode
// providers, so the client never sees the upstream credentials.
//
// State (files, batches, sessions) is NOT synchronised across cluster
// nodes — a request creating state on one backend is not replicated to
// another. Same constraint that applies to OpenAICompat.DefaultBackend.
type NativeWireConfig struct {
	// Mounts maps the mount path (without leading slash) to the provider
	// key in provider config. Keys are matched against the first path
	// segment of the incoming request; the remainder is forwarded to the
	// backend unchanged.
	Mounts map[string]string `mapstructure:"mounts" yaml:"mounts,omitempty"`
}

// ProvidersConfig holds node-level install knobs. Empty values leave each
// subsystem at its built-in default (XDG provider dir, no proxy, harden=true).
type ProvidersConfig struct {
	// InstallPolicyFile is startup-only human-owned override authority.
	InstallPolicyFile string `mapstructure:"install_policy_file" yaml:"install_policy_file,omitempty"`
	// InstallDir overrides install.ProviderRootDir(). Accepts an absolute
	// path; empty means keep the platform default (Linux: /opt/zzrouter,
	// macOS: ~/Library/Application Support/zzrouter/providers, Windows:
	// %LOCALAPPDATA%\zzrouter\providers). Useful when the default location
	// lives on a space-constrained or non-ROOT-writable filesystem.
	InstallDir string `mapstructure:"install_dir" yaml:"install_dir,omitempty"`

	// Proxy is the optional HTTP proxy + CA bundle applied to every
	// provider install step (curl downloads, pip installs, PowerShell
	// Invoke-WebRequest). All fields are optional.
	Proxy ProxyConfig `mapstructure:"proxy" yaml:"proxy,omitempty"`

	// HardenVenv controls the chmod -R a-w step on Python venvs after
	// pip install. Default (nil) = true. Set false on hosts where the
	// venv has mixed ownership (shared pip cache, service-user installs)
	// and the hardening step would legitimately fail.
	HardenVenv *bool `mapstructure:"harden_venv" yaml:"harden_venv,omitempty"`
}

// ProxyConfig carries HTTP proxy + CA-bundle settings for install steps.
// Values are passed through as HTTP_PROXY / HTTPS_PROXY / NO_PROXY /
// SSL_CERT_FILE / CURL_CA_BUNDLE / PIP_CERT env vars on install subprocesses.
type ProxyConfig struct {
	// HTTPProxy populates HTTP_PROXY and http_proxy.
	HTTPProxy string `mapstructure:"http_proxy" yaml:"http_proxy,omitempty"`
	// HTTPSProxy populates HTTPS_PROXY and https_proxy.
	HTTPSProxy string `mapstructure:"https_proxy" yaml:"https_proxy,omitempty"`
	// NoProxy populates NO_PROXY and no_proxy (comma-separated hostnames).
	NoProxy string `mapstructure:"no_proxy" yaml:"no_proxy,omitempty"`
	// CAFile is an absolute path to a PEM bundle trusted by the corporate
	// proxy. Populates SSL_CERT_FILE (OpenSSL), CURL_CA_BUNDLE (curl), and
	// PIP_CERT (pip). Leave empty to use the system trust store.
	CAFile string `mapstructure:"ca_file" yaml:"ca_file,omitempty"`
}

// IsSet reports whether any proxy knob carries a non-empty value.
func (p *ProxyConfig) IsSet() bool {
	return p.HTTPProxy != "" || p.HTTPSProxy != "" || p.NoProxy != "" || p.CAFile != ""
}

// ShouldHardenVenv returns the effective HardenVenv value, defaulting to true.
func (p *ProvidersConfig) ShouldHardenVenv() bool {
	if p.HardenVenv == nil {
		return true
	}
	return *p.HardenVenv
}

// applyLoadDefaults fills in the fields that are derived rather than
// configured. Every load path runs it, so no consumer has to defend
// against a zero value the loader could have resolved for it.
func (c *NodeConfig) applyLoadDefaults() {
	// node.name is documented as "defaults to hostname", but the
	// defaulting used to live in the CLI. A node started as a service
	// skipped it and then advertised itself to the cluster by URL
	// instead of by name.
	if c.Node.Name == "" {
		if hostname, err := os.Hostname(); err == nil {
			c.Node.Name = hostname
		}
	}
	c.MigrateDeprecatedFields()
}

// MigrateDeprecatedFields promotes old top-level routing/chat into the
// Coordinator section when the new section is empty. This keeps existing
// node.yaml files working without modification.
func (c *NodeConfig) MigrateDeprecatedFields() {
	// routing → coordinator.routing
	if c.Coordinator.Routing.DefaultMode == "" &&
		c.Coordinator.Routing.RoutePrefix == nil &&
		!c.Coordinator.Routing.InjectUsageMetadata &&
		len(c.Coordinator.Routing.ModelRouting) == 0 &&
		len(c.Coordinator.Routing.FormatPriorities) == 0 {
		if c.Routing.DefaultMode != "" ||
			c.Routing.RoutePrefix != nil ||
			c.Routing.InjectUsageMetadata ||
			len(c.Routing.ModelRouting) > 0 ||
			len(c.Routing.FormatPriorities) > 0 {
			c.Coordinator.Routing = c.Routing
		}
	}

	// chat → coordinator.chat
	if c.Coordinator.Chat.SlashCommands.Enabled == nil &&
		c.Coordinator.Chat.SlashCommands.Prefix == "" {
		if c.Chat.SlashCommands.Enabled != nil || c.Chat.SlashCommands.Prefix != "" {
			c.Coordinator.Chat = c.Chat
		}
	}
}

// Validate checks the NodeConfig for production readiness issues.
func (c *NodeConfig) Validate() error {
	if c.Node.Port < 1 || c.Node.Port > 65535 {
		return fmt.Errorf("%w: node.port must be between 1 and 65535, got %d", ErrInvalidConfig, c.Node.Port)
	}

	if c.Auth.AdminKey != "" && len(c.Auth.AdminKey) < MinAPIKeyLength {
		return fmt.Errorf("%w: auth.admin_key must be at least %d characters (got %d)", ErrInvalidConfig, MinAPIKeyLength, len(c.Auth.AdminKey))
	}
	if c.Auth.UserKey != "" && len(c.Auth.UserKey) < MinAPIKeyLength {
		return fmt.Errorf("%w: auth.user_key must be at least %d characters (got %d)", ErrInvalidConfig, MinAPIKeyLength, len(c.Auth.UserKey))
	}

	// cluster.endpoints is the coordinator's list of the workers it
	// manages, so a worker must not carry one. A worker learns its
	// coordinator by pairing, not from config.
	if c.Cluster.Mode == ClusterModeWorker && len(c.Cluster.Endpoints) > 0 {
		return fmt.Errorf("%w: cluster.endpoints must be empty in worker mode (a worker learns its coordinator by pairing)", ErrInvalidConfig)
	}

	// TLS validation: cert and key must both be set or both empty
	if (c.Node.TLSCert == "") != (c.Node.TLSKey == "") {
		return fmt.Errorf("%w: node.tls_cert and node.tls_key must both be set (or both empty)", ErrInvalidConfig)
	}

	if err := c.Security.CORS.Validate(); err != nil {
		return err
	}

	return nil
}
