// Package config provides OS-agnostic configuration file management for zzRouter.
//
// Configuration types are organised by domain across per-concern files:
//
//   - node_config.go          — NodeConfig and its MCP/NativeWire/Providers sub-structs
//   - client_config.go        — ClientNodeConfig, CacheConfig, ModelsConfig, PricingConfig, ClientConfig
//   - serve_config.go         — ServeConfig (HTTP server), AdminConfig, UserConfig, AuthConfig
//   - security_config.go      — SecurityConfig, CORSConfig (+ validation)
//   - provider_config.go      — ProviderConfig, LaunchConfig (legacy node.yaml shape)
//   - cluster_config.go       — ClusterConfig, ClusterMode, StrategyDefault
//   - cli_config.go           — CLIConfig, NodeConnection, ClientPreferences, Chat/Search settings
//   - observability_config.go — ObservabilityConfig, OTLPConfig, TracingConfig, MetricsConfig
//   - routing_config.go       — RoutingConfig, ResourceThresholds, NodePriority, ModelRoutingRule
//   - chat_config.go          — ChatConfig, SlashCommandsConfig
//   - update_config.go        — UpdateConfig, InferenceLogConfig, cron/version validators
//
// Cross-cutting helpers live in their own files: security.go (API key utilities),
// provider.go (provider Kind discriminator), service_config.go (ServiceConfig
// methods), overrides.go, paths.go, manager.go, etc.
package config
