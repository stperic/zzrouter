package client

import (
	"errors"
	"net/url"
	"strings"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ProviderStatusResponse is the client representation of a provider's status.
type ProviderStatusResponse struct {
	Name            string             `json:"name"`
	Status          string             `json:"status"`
	CooldownSeconds float64            `json:"cooldown_seconds,omitempty"`
	CooldownReason  string             `json:"cooldown_reason,omitempty"`
	RateLimit       *RateLimitResponse `json:"rate_limit,omitempty"`
}

// RateLimitResponse is the client representation of a provider's last-seen rate limit headers.
type RateLimitResponse struct {
	LimitRequests        int     `json:"limit_requests,omitempty"`
	RemainingRequests    int     `json:"remaining_requests,omitempty"`
	LimitTokens          int     `json:"limit_tokens,omitempty"`
	RemainingTokens      int     `json:"remaining_tokens,omitempty"`
	ResetRequestsSeconds float64 `json:"reset_requests_seconds,omitempty"`
	ResetTokensSeconds   float64 `json:"reset_tokens_seconds,omitempty"`
	ObservedAt           string  `json:"observed_at"`
	ObservedAgoSeconds   float64 `json:"observed_ago_seconds"`
}

// ProviderStatusListResponse is the envelope for GET /providers/status.
type ProviderStatusListResponse struct {
	Data    []ProviderStatusResponse `json:"data"`
	Total   int                      `json:"total"`
	HasMore bool                     `json:"has_more,omitempty"`
}

// ListProviderStatus retrieves status for all providers.
// GET /zzrouter/v1/providers/status
func (c *Client) ListProviderStatus() ([]ProviderStatusResponse, error) {
	var result ProviderStatusListResponse
	if err := c.doJSON("GET", apipath.ProvidersStatus, nil, &result, "list provider status"); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// ============================================================================
// Provider Catalog
// ============================================================================

// ProviderCatalogEntry represents a provider definition from provider config.
type ProviderCatalogEntry struct {
	Key             string   `json:"key"`
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	Mode            string   `json:"mode"`
	Protocol        string   `json:"protocol"`
	IsCloud         bool     `json:"is_cloud"`
	Installed       bool     `json:"installed"`
	Available       bool     `json:"available"`
	RequiredEnvVars []string `json:"required_env_vars,omitempty"`
}

// GetProviderCatalog retrieves all defined providers from provider config.
// GET /zzrouter/v1/providers/catalog
func (c *Client) GetProviderCatalog() ([]ProviderCatalogEntry, error) {
	var result struct {
		Data []ProviderCatalogEntry `json:"data"`
	}
	if err := c.doJSON("GET", apipath.ProvidersCatalog, nil, &result, "get provider catalog"); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// ============================================================================
// Provider Verification
// ============================================================================

// VerifyProviderResponse is the result of a provider connectivity check.
type VerifyProviderResponse struct {
	Provider string `json:"provider"`
	Status   string `json:"status"` // "ok" or "failed"
	Message  string `json:"message"`
}

// VerifyProvider tests connectivity to a provider using its configured endpoint and auth.
// POST /zzrouter/v1/providers/:name/verify
func (c *Client) VerifyProvider(name string) (*VerifyProviderResponse, error) {
	var envelope struct {
		Data VerifyProviderResponse `json:"data"`
	}
	if err := c.doJSON("POST", apipath.ProviderVerify(name), nil, &envelope, "verify provider"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ============================================================================
// Preflight Types
// ============================================================================

// PreflightResponse is the client representation of a preflight report.
type PreflightResponse struct {
	Provider string           `json:"provider"`
	Results  []PreflightCheck `json:"results"`
	AllOK    bool             `json:"all_ok"`
}

// PreflightCheck is a single preflight check result.
type PreflightCheck struct {
	Check   string `json:"check"`
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// GetPreflight runs preflight checks for a provider.
// POST /zzrouter/v1/providers/:name/install/preflight?node=<node>
func (c *Client) GetPreflight(name, node string) (*PreflightResponse, error) {
	path := apipath.ProviderInstallPreflight(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	var envelope struct {
		Data PreflightResponse `json:"data"`
	}
	if err := c.doJSON("POST", path, nil, &envelope, "preflight check"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ============================================================================
// Install Plan Types
// ============================================================================

// InstallPlanResponse is the client representation of an install plan.
type InstallPlanResponse struct {
	PlanID       string              `json:"plan_id,omitempty"`
	Provider     string              `json:"provider"`
	Version      string              `json:"version"`
	Action       string              `json:"action"`
	InstallDir   string              `json:"install_dir"`
	Steps        []InstallStepInfo   `json:"steps"`
	CurrentState []StepStateResponse `json:"current_state,omitempty"`
}

// InstallStepInfo represents a single step in an install plan.
type InstallStepInfo struct {
	Number      int    `json:"step"`
	Description string `json:"description"`
	Command     string `json:"command"`
	Notes       string `json:"notes,omitempty"`
}

// StepStateResponse reports the pre-install state of a step.
// `Installed: false` on a fresh machine is the expected state, not a failure.
type StepStateResponse struct {
	Step      int    `json:"step"`
	Installed bool   `json:"installed"`
	Detail    string `json:"detail,omitempty"`
}

// StepResultResponse is the outcome of executing or verifying a single install
// step. Used on the execute-step and verify-step endpoints, where pass/fail
// is a real judgment (as opposed to plan-preview, which uses StepStateResponse).
type StepResultResponse struct {
	Step    int    `json:"step"`
	Passed  bool   `json:"passed"`
	Actual  string `json:"actual,omitempty"`
	Message string `json:"message"`
}

// ============================================================================
// Install Plan API
// ============================================================================

// GetInstallPlan retrieves the install plan for a provider.
// POST /zzrouter/v1/providers/:name/install/plan?node=<node>
func (c *Client) GetInstallPlan(name, node string) (*InstallPlanResponse, error) {
	path := apipath.ProviderInstallPlan(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	var envelope struct {
		Data InstallPlanResponse `json:"data"`
	}
	if err := c.doJSON("POST", path, nil, &envelope, "get install plan"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ExecuteStepAcceptedResponse is the 202 body for async execute-step:
// {accepted, job_id, provider, step, node}. Subscribe to job_id via
// /zzrouter/v1/jobs/:id/stream for per-step progress; the terminal
// StepResult is carried in the final event's Meta.
type ExecuteStepAcceptedResponse struct {
	Accepted bool   `json:"accepted"`
	JobID    string `json:"job_id"`
	Provider string `json:"provider"`
	Step     int    `json:"step"`
	Node     string `json:"node"`
}

// ExecuteInstallStep runs a single step in an install plan. Async:
// returns the acceptance envelope carrying the job_id; the step runs
// on a server goroutine. Subscribers follow up via the jobs stream
// for progress + terminal StepResult.
// POST /zzrouter/v1/providers/:name/install/execute-step?node=<node> → 202.
func (c *Client) ExecuteInstallStep(name, node string, step int, plans ...*InstallPlanResponse) (*ExecuteStepAcceptedResponse, error) {
	path := apipath.ProviderInstallExecuteStep(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	var envelope struct {
		Data ExecuteStepAcceptedResponse `json:"data"`
	}
	body := map[string]any{"step": step}
	if len(plans) > 0 && plans[0] != nil {
		body["expected_plan_id"] = plans[0].PlanID
		body["version"] = plans[0].Version
		body["action"] = plans[0].Action
		body["runtime"] = plans[0].Provider
	}
	if err := c.doJSON("POST", path, body, &envelope, "execute install step"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// VerifyInstallStep verifies a single step in an install plan.
// POST /zzrouter/v1/providers/:name/install/verify-step?node=<node>
func (c *Client) VerifyInstallStep(name, node string, step int, plans ...*InstallPlanResponse) (*StepResultResponse, error) {
	path := apipath.ProviderInstallVerifyStep(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	var envelope struct {
		Data StepResultResponse `json:"data"`
	}
	body := map[string]any{"step": step}
	if len(plans) > 0 && plans[0] != nil {
		body["expected_plan_id"] = plans[0].PlanID
		body["version"] = plans[0].Version
		body["action"] = plans[0].Action
		body["runtime"] = plans[0].Provider
	}
	if err := c.doJSON("POST", path, body, &envelope, "verify install step"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ============================================================================
// Install / Uninstall
// ============================================================================

// InstallProgress is the client-side mirror of install.InstallProgress.
// All fields match the server JSON; kept in a client struct so the TUI
// doesn't need to import pkg/prov_apps/install.
type InstallProgress struct {
	Provider   string `json:"provider"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Step       int    `json:"step"`
	TotalSteps int    `json:"total_steps"`
	StepDesc   string `json:"step_description"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
	BytesDone  int64  `json:"bytes_done,omitempty"`
	Percent    int    `json:"percent"`
	Error      string `json:"error,omitempty"`
}

// InstallAcceptedResponse is the 202 body from the async install contract:
// {accepted, job_id, provider, node}. Subscribe to job_id via
// /zzrouter/v1/jobs/:id/stream?node=<node> for progress.
type InstallAcceptedResponse struct {
	Accepted bool   `json:"accepted"`
	JobID    string `json:"job_id"`
	Provider string `json:"provider"`
	Node     string `json:"node"`
}

// InstallProvider installs a provider on a specific node. Returns the
// async acceptance envelope (includes JobID) — the install runs in a
// server-side goroutine; callers should subscribe to the job for the
// terminal outcome.
// POST /zzrouter/v1/providers/:name/install?node=<node> → 202.
func (c *Client) InstallProvider(name, node string, plans ...*InstallPlanResponse) (*InstallAcceptedResponse, error) {
	path := apipath.ProviderInstall(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	var envelope struct {
		Data InstallAcceptedResponse `json:"data"`
	}
	var body any
	if len(plans) > 0 && plans[0] != nil {
		body = map[string]any{"expected_plan_id": plans[0].PlanID, "version": plans[0].Version, "runtime": plans[0].Provider, "action": plans[0].Action}
	}
	if err := c.doJSON("POST", path, body, &envelope, "install provider"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// UpgradeProvider upgrades a provider on a specific node. An empty version
// upgrades to the newest release the provider's upstream publishes; a
// non-empty version installs exactly that.
//
// Upgrade does NOT resolve to the config pin. The pin seeds fresh installs,
// so resolving to it here would make upgrade a no-op whenever the node
// already matches, and a downgrade after an earlier one-off upgrade. Use
// SetProviderPinnedVersion to change what new nodes start on.
// POST /zzrouter/v1/providers/:name/upgrade?node=<node> → 202.
func (c *Client) UpgradeProvider(name, version, node string) (*InstallAcceptedResponse, error) {
	path := apipath.ProviderUpgrade(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	body := map[string]string{"version": version}
	var envelope struct {
		Data InstallAcceptedResponse `json:"data"`
	}
	if err := c.doLongJSON("POST", path, body, &envelope, "upgrade provider"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// SetProviderPinnedVersion durably pins a provider to an install version.
// An empty version clears the pin, restoring "resolve latest upstream".
// The coordinator persists the pin and fans the new config out to workers.
// PATCH /zzrouter/v1/providers/:name.
func (c *Client) SetProviderPinnedVersion(name, version, node string) error {
	path := apipath.Provider(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	body := map[string]any{"pinned_version": version}
	return c.doJSON("PATCH", path, body, nil, "set provider version pin")
}

// AddProviderInstanceRequest is the body of POST /zzrouter/v1/providers/instances.
// Today the only supported type is "ollama-connect".
type AddProviderInstanceRequest struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token,omitempty"`
}

// AddProviderInstanceResponse carries the minimal success payload the
// server emits after creating a runtime provider instance.
type AddProviderInstanceResponse struct {
	Data struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Kind     string `json:"kind"`
		Protocol string `json:"protocol"`
		Endpoint string `json:"endpoint"`
		Enabled  bool   `json:"enabled"`
	} `json:"data"`
}

// AddProviderInstance creates a runtime provider instance (e.g. an ollama
// connect entry pointing at a LAN or remote daemon). The server writes the
// new entry to the providers directory atomically and fires its OnChange
// listeners so the model registry scans the new backend on next refresh.
func (c *Client) AddProviderInstance(req AddProviderInstanceRequest) (*AddProviderInstanceResponse, error) {
	var out AddProviderInstanceResponse
	if err := c.doJSON("POST", apipath.ProvidersInstances, req, &out, "add provider instance"); err != nil {
		return nil, err
	}
	return &out, nil
}

// UninstallProvider removes a provider from a specific node.
// DELETE /zzrouter/v1/providers/:name?node=<node>
func (c *Client) UninstallProvider(name, node string) error {
	path := apipath.Provider(name)
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	return c.doLongJSON("DELETE", path, nil, nil, "uninstall provider")
}

// ============================================================================
// Upstream Versions
// ============================================================================

// NodeVersionInfo is one node's installed version and how it compares to the
// newest release upstream publishes.
type NodeVersionInfo struct {
	Node      string `json:"node"`
	Installed string `json:"installed,omitempty"`
	Status    string `json:"status"`
	// InstallableLatest is the newest release THIS node can install, and
	// InstallableSource names the upstream that caps it. Both are present
	// only when that differs from the report's headline. Prefer them: the
	// headline names what the project published, which a node installing
	// through a packager cannot necessarily fetch.
	InstallableLatest string `json:"installable_latest,omitempty"`
	InstallableSource string `json:"installable_source,omitempty"`
}

// VersionSourceInfo echoes the configured upstream feed.
type VersionSourceInfo struct {
	Type    string `json:"type"`
	Repo    string `json:"repo,omitempty"`
	Package string `json:"package,omitempty"`
}

// ProviderVersionsResponse reports what upstream publishes against what is
// pinned and what each node runs.
//
// Latest empty with Reason set is the normal shape for an untracked provider,
// a cluster with checks disabled, or an unreachable upstream. It is never an
// error: the installed versions stay useful either way.
type ProviderVersionsResponse struct {
	Provider   string             `json:"provider"`
	Latest     string             `json:"latest,omitempty"`
	LatestTag  string             `json:"latest_tag,omitempty"`
	ReleaseURL string             `json:"release_url,omitempty"`
	CheckedAt  string             `json:"checked_at,omitempty"`
	Source     *VersionSourceInfo `json:"source,omitempty"`

	Pinned       string `json:"pinned,omitempty"`
	PinnedStatus string `json:"pinned_status"`

	Nodes  []NodeVersionInfo `json:"nodes"`
	Reason string            `json:"reason,omitempty"`
}

// GetProviderVersions fetches the upstream release report for a provider.
// Pass refresh to bypass the coordinator's cache.
// GET /zzrouter/v1/providers/:name/versions.
func (c *Client) GetProviderVersions(name string, refresh bool) (*ProviderVersionsResponse, error) {
	path := apipath.ProviderVersionsFor(name)
	if refresh {
		path += "?refresh=true"
	}
	var envelope struct {
		Data ProviderVersionsResponse `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &envelope, "provider versions"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ============================================================================
// Model Usage
// ============================================================================

// ModelUsage is one model's running usage since the node started.
//
// Priced reports whether any request carried an authoritative price. A
// self-hosted model has none, so CostUSD stays absent rather than reading as
// a $0.00 that looks like a bug.
type ModelUsage struct {
	Model    string   `json:"model"`
	Provider string   `json:"provider,omitempty"`
	Nodes    []string `json:"nodes,omitempty"`

	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors,omitempty"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`

	CostUSD float64 `json:"cost_usd,omitempty"`
	Priced  bool    `json:"priced"`

	AvgTokensPerSec  float64 `json:"avg_tokens_per_sec,omitempty"`
	PeakTokensPerSec float64 `json:"peak_tokens_per_sec,omitempty"`
	AvgLatencyMs     float64 `json:"avg_latency_ms,omitempty"`

	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// ModelUsageResponse carries either a single model or the full list, plus the
// window the totals cover.
type ModelUsageResponse struct {
	Since      string       `json:"since"`
	UptimeSecs float64      `json:"uptime_seconds"`
	Models     []ModelUsage `json:"models,omitempty"`
	Model      *ModelUsage  `json:"model,omitempty"`
}

// ErrNoUsageRecorded means the node has served no request for that model
// since it started. Distinct from a transport failure: one means "nothing has
// happened yet", the other means "we could not find out", and a usage panel
// must not render the second as the first.
var ErrNoUsageRecorded = errors.New("no usage recorded since node start")

// GetModelUsage fetches running usage for one model since node start. An
// empty provider sums every provider serving that model name.
//
// Returns ErrNoUsageRecorded when the node has served nothing for that model,
// which is a reportable state rather than a failure.
// GET /zzrouter/v1/usage/models/:model.
func (c *Client) GetModelUsage(model, provider string) (*ModelUsageResponse, error) {
	path := apipath.UsageModel(model)
	if provider != "" {
		path += "?provider=" + url.QueryEscape(provider)
	}
	var envelope struct {
		Data ModelUsageResponse `json:"data"`
	}
	if err := c.doJSON("GET", path, nil, &envelope, "model usage"); err != nil {
		// doJSON folds the status into the message, so match on it here
		// rather than teaching every caller to parse the string.
		if strings.Contains(err.Error(), "status 404") {
			return nil, ErrNoUsageRecorded
		}
		return nil, err
	}
	return &envelope.Data, nil
}

// ListModelUsage fetches running usage for every model seen since node start.
// GET /zzrouter/v1/usage/models.
func (c *Client) ListModelUsage() (*ModelUsageResponse, error) {
	var envelope struct {
		Data ModelUsageResponse `json:"data"`
	}
	if err := c.doJSON("GET", apipath.UsageModels, nil, &envelope, "model usage"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}
