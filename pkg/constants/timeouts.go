package constants

import "time"

// ======================================================================================
// Timeout Constants - Centralized timeout configuration for the entire application
// ======================================================================================
// This file is the SINGLE SOURCE OF TRUTH for all timeout values.
// Organized by category for easy maintenance and understanding.
// ======================================================================================

// --------------------------------------------------------------------------------------
// HTTP Client Timeouts
// --------------------------------------------------------------------------------------

const (
	// HTTPShortTimeout is for quick operations that should fail fast
	// Used for: Health checks, status queries
	HTTPShortTimeout = 5 * time.Second

	// HTTPDefaultTimeout is for normal HTTP operations
	// Used for: API calls, metadata queries
	HTTPDefaultTimeout = 30 * time.Second

	// HTTPLongTimeout is for operations that may take time
	// Used for: Model loading, large data transfers
	HTTPLongTimeout = 120 * time.Second

	// HTTPDownloadTimeout is for downloading large files
	// Used for: Model downloads from HuggingFace, etc.
	HTTPDownloadTimeout = 120 * time.Minute
)

// --------------------------------------------------------------------------------------
// Cluster Operation Timeouts
// --------------------------------------------------------------------------------------

const (
	// ClusterHealthCheckTimeout is for quick health checks during discovery
	// Used for: Initial cluster node detection, fast-fail scenarios
	ClusterHealthCheckTimeout = 2 * time.Second

	// ClusterQueryTimeout is for read operations across the cluster
	// Used for: GET /models, GET /runs, aggregating data
	ClusterQueryTimeout = 10 * time.Second

	// ClusterActionTimeout is for write operations (stop, restart, load)
	// Used for: DELETE /runs/:id, POST /runs/:id/restart, POST /models/load
	ClusterActionTimeout = 30 * time.Second

	// ClusterDefaultTimeout is the maximum timeout for cluster operations
	// Used for: Long-running operations, model loading across cluster
	ClusterDefaultTimeout = 5 * time.Minute
)

// --------------------------------------------------------------------------------------
// Service Discovery Timeouts
// --------------------------------------------------------------------------------------

const (
	// DiscoveryTimeout is for service discovery operations
	// Used for: mDNS discovery, network scanning
	DiscoveryTimeout = 10 * time.Second

	// VersionDiscoveryTimeout is for version detection of services
	// Used for: Detecting Ollama/vLLM/llama.cpp versions
	VersionDiscoveryTimeout = 10 * time.Minute
)

// --------------------------------------------------------------------------------------
// Health Check & Monitoring Timeouts
// --------------------------------------------------------------------------------------

const (
	// HealthCheckTimeout is for provider health checks
	// Used for: Checking if Ollama/vLLM/etc. are responsive
	HealthCheckTimeout = 10 * time.Second

	// HealthCheckInterval is how often to check health
	// Used for: Health monitor polling interval
	HealthCheckInterval = 30 * time.Second

	// HealthCheckStartupGrace is how long to wait before first health check
	// Used for: Allowing process to initialize before checking health
	HealthCheckStartupGrace = 60 * time.Second

	// StartupTimeout is the maximum time to wait for a service to start
	// Used for: Waiting for Ollama/vLLM to become ready
	StartupTimeout = 2 * time.Minute

	// RetryInitialDelay is the initial delay between retry attempts
	RetryInitialDelay = 5 * time.Second

	// RetryMaxDelay is the maximum delay between retry attempts
	RetryMaxDelay = 60 * time.Second
)

// --------------------------------------------------------------------------------------
// Process Management Timeouts
// --------------------------------------------------------------------------------------

const (
	// ProcessGracefulTimeout is time allowed for graceful shutdown (SIGTERM)
	ProcessGracefulTimeout = 3 * time.Second

	// ProcessForceTimeout is time allowed after SIGKILL before giving up
	ProcessForceTimeout = 3 * time.Second

	// ProcessGPUGracefulTimeout is longer graceful timeout for GPU providers
	// (MLX/Metal, vLLM/CUDA, llama.cpp) — need time to release GPU memory
	ProcessGPUGracefulTimeout = 5 * time.Second

	// ProcessGPUForceTimeout is longer force timeout for GPU providers
	ProcessGPUForceTimeout = 5 * time.Second

	// ProcessDefaultStopTimeout is default timeout when stopping instances
	ProcessDefaultStopTimeout = 3 * time.Second

	// ShutdownMaxTimeout caps the total shutdown time to prevent indefinite hangs.
	// Sits comfortably inside systemd's default TimeoutStopSec=90s so the OS
	// won't SIGKILL mid-flush under heavy HTTP drain. Spend state is pre-drained
	// to disk before HTTP drain starts, so this cap governs HTTP and goroutine
	// teardown only.
	ShutdownMaxTimeout = 45 * time.Second

	// RoleDemoteTimeout bounds the coordinator-subsystem teardown when the
	// role manager transitions the node out of coordinator mode. Kept tight
	// so a flapping role can't stall promote/demote — unlike full shutdown,
	// no spend flush is on the critical path here.
	RoleDemoteTimeout = 30 * time.Second

	// ProcessWaitTimeout is time to wait for process.Wait()
	// Used for: Ensuring process cleanup completes
	ProcessWaitTimeout = 2 * time.Second
)

// --------------------------------------------------------------------------------------
// Search & External API Timeouts
// --------------------------------------------------------------------------------------

const (
	// SearchTimeout is for search operations
	// Used for: HuggingFace search, model discovery
	SearchTimeout = 5 * time.Second

	// HuggingFaceAPITimeout is for HuggingFace API calls
	// Used for: Fetching model metadata, model cards
	HuggingFaceAPITimeout = 30 * time.Second
)

// --------------------------------------------------------------------------------------
// Instance Lifecycle Timeouts
// --------------------------------------------------------------------------------------

const (
	// InstanceDefaultKeepAlive is default idle time before auto-stopping on-demand instances
	InstanceDefaultKeepAlive = 5 * time.Minute

	// InstanceKeepAliveCheckInterval is how often to check for idle instances
	InstanceKeepAliveCheckInterval = 30 * time.Second

	// InstanceFailedTTL is how long to keep failed instance records before cleanup
	InstanceFailedTTL = 60 * time.Minute

	// InstanceStartupTimeout is max time to wait for instance to become healthy
	InstanceStartupTimeout = 2 * time.Minute

	// InstanceHealthProbeInterval is how often to poll health during startup
	InstanceHealthProbeInterval = 1 * time.Second
)

// --------------------------------------------------------------------------------------
// Log Management
// --------------------------------------------------------------------------------------

const (
	// LogMaxAge is how long to keep log files before cleanup
	LogMaxAge = 7 * 24 * time.Hour // 7 days
)

// --------------------------------------------------------------------------------------
// Service Management (Windows SCM)
// --------------------------------------------------------------------------------------

const (
	// WindowsServiceStopTimeout is max time to wait for Windows service to stop
	WindowsServiceStopTimeout = 30 * time.Second

	// WindowsServicePollInterval is how often to poll service status
	WindowsServicePollInterval = 500 * time.Millisecond
)

// --------------------------------------------------------------------------------------
// Polling Intervals
// --------------------------------------------------------------------------------------

const (
	// SSEPollInterval is for Server-Sent Events streaming
	// Used for: Log streaming, real-time updates
	SSEPollInterval = 50 * time.Millisecond

	// StatusPollInterval is for polling operation status
	// Used for: Checking async operation completion
	StatusPollInterval = 100 * time.Millisecond

	// LogStreamPollInterval is for log streaming operations
	// Used for: Tailing logs, streaming output
	LogStreamPollInterval = 200 * time.Millisecond

	// ProgressPollInterval is for progress updates
	// Used for: Download progress, model loading progress
	ProgressPollInterval = 500 * time.Millisecond

	// DownloadPollInterval is for download status polling
	// Used for: Checking download completion
	DownloadPollInterval = 2 * time.Second
)

// --------------------------------------------------------------------------------------
// Background Task Intervals
// --------------------------------------------------------------------------------------

const (
	// ResourceTrackerInterval is how often to update resource tracking
	// Used for: Cluster resource monitoring
	ResourceTrackerInterval = 30 * time.Second

	// PeriodicScanInterval is for periodic background scans
	// Used for: Model registry sync, cache refresh
	PeriodicScanInterval = 5 * time.Minute

	// CacheRefreshInterval is for cache TTL/refresh
	// Used for: Health check cache, model cache
	CacheRefreshInterval = 30 * time.Second

	// MaintenanceInterval is how often the server maintenance loop runs
	// Used for: Cleaning completed jobs, expired failures, stale entries, old logs
	MaintenanceInterval = 15 * time.Minute
)

// --------------------------------------------------------------------------------------
// HTTP Server Timeouts
// --------------------------------------------------------------------------------------

const (
	// ServerReadHeaderTimeout is the max time to read request headers
	// Prevents slow-loris attacks and zombie connections from exhausting file descriptors
	// NOTE: WriteTimeout/ReadTimeout are NOT set globally due to SSE/streaming endpoints
	ServerReadHeaderTimeout = 10 * time.Second

	// ServerIdleTimeout is how long to keep idle keep-alive connections open
	// Prevents connection accumulation from idle clients
	ServerIdleTimeout = 120 * time.Second
)

// --------------------------------------------------------------------------------------
// Configurator Timeouts
// --------------------------------------------------------------------------------------

const (
	// ConfiguratorTimeout is for configurator operations
	// Used for: Interactive setup, configuration validation
	ConfiguratorTimeout = 60 * time.Second

	// ConfiguratorLongTimeout is for long configurator operations
	// Used for: Provider discovery, template application
	ConfiguratorLongTimeout = 120 * time.Second
)

// --------------------------------------------------------------------------------------
// Streaming & Real-time Timeouts
// --------------------------------------------------------------------------------------

const (
	// StreamingTimeout is for streaming operations
	// Used for: SSE connections, log streaming
	StreamingTimeout = 5 * time.Minute

	// ModelStreamingTimeout is for model inference streaming
	// Used for: Chat completions with streaming
	ModelStreamingTimeout = 120 * time.Second

	// ModelLoadGrace is how long a streaming request waits for a cold
	// start before giving up on a real HTTP status and committing to
	// 200 + SSE. Under it, a load failure reaches the caller as a status
	// code and a load success streams through the same path a warm
	// request uses. Over it, the caller gets progress frames instead —
	// worth more than a status code once the connection would otherwise
	// sit silent long enough to look hung.
	//
	// Sized off measurements on this hardware: a 9B MLX cold start takes
	// ~4s and a model that cannot load fails in ~1.5s, so the common
	// cases resolve well inside it while a genuinely slow load still
	// gets its feedback.
	ModelLoadGrace = 15 * time.Second

	// ModelLoadHeartbeat is how often a committed stream emits an SSE
	// comment while it waits for a slow cold start. Once the grace has
	// expired the response is already 200 + text/event-stream, and it
	// can then sit silent for the rest of ModelStreamingTimeout — long
	// enough for a proxy's idle timeout to kill a load that was going to
	// succeed. A comment rather than a status frame because comments are
	// ignored by every conforming SSE parser, so the keepalive cannot be
	// mistaken for a chat chunk.
	ModelLoadHeartbeat = 10 * time.Second
)
