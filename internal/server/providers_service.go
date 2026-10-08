package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/routing"
)

// ============================================================================
// Providers Service - Business Logic Layer with Cluster Routing
// ============================================================================
//
// Responsibilities:
// - Route provider lifecycle requests to the correct node
// - No HTTP concerns (that's the controller's job)
// - No direct provider calls (that's the executor's job)

// ProvidersService handles provider lifecycle operations with cluster routing.
type ProvidersService struct {
	router routing.Router
}

// NewProvidersService creates a new providers service.
func NewProvidersService(router routing.Router) *ProvidersService {
	return &ProvidersService{router: router}
}

// localIfEmpty returns "localhost" when node is empty, ensuring mutation
// operations (install, upgrade, uninstall) target the local node by default.
// This prevents accidental broadcasts for destructive operations.
//
// For read operations (ListProviders), an empty node uses the default route
// behavior. Callers should set ?node=<name> to target a specific remote node.
func localIfEmpty(node string) string {
	if node == "" {
		return "localhost"
	}
	return node
}

// --- Status ---

func (s *ProvidersService) GetProviderStatus(ctx context.Context, name, node string) (*prov_apps.ProviderStatus, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()

	return routeAndParse[prov_apps.ProviderStatus](ctx, s.router, "GET",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/status", name), localIfEmpty(node), nil)
}

// GetProviderEnvironment routes a fixed read-only probe to exactly one node.
func (s *ProvidersService) GetProviderEnvironment(ctx context.Context, provider, runtime, node string) (*providerEnvironment, error) {
	ctx, cancel := ensureTimeout(ctx, LongRequestTimeout)
	defer cancel()
	path := "/zzrouter/v1/internal/providers/" + url.PathEscape(provider) + "/environment"
	if runtime != "" {
		path += "?runtime=" + url.QueryEscape(runtime)
	}
	result, err := routeAndParse[providerEnvironment](ctx, s.router, "GET", path, localIfEmpty(node), nil)
	if err != nil {
		return nil, err
	}
	if result.Contract != environmentContract {
		return nil, fmt.Errorf("node environment contract unsupported; upgrade the node")
	}
	return result, nil
}

// --- Install Status (polling) ---

// GetInstallStatus fetches the current install progress snapshot for a
// provider on the target node. Returns (nil, nil) if no install is running.
func (s *ProvidersService) GetInstallStatus(ctx context.Context, name, node string) (*install.InstallProgress, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()

	raw, err := s.routeRaw(ctx, "GET",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/status", name), localIfEmpty(node), nil)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var p install.InstallProgress
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// --- Install ---

func (s *ProvidersService) InstallProvider(ctx context.Context, provider, version, node string, force bool, options ...install.PlanOptions) (json.RawMessage, error) {
	ctx, cancel := ensureTimeout(ctx, constants.HTTPDownloadTimeout)
	defer cancel()

	body, _ := json.Marshal(installRequest{Version: version, PlanOptions: selectPlanOptions(options, force)})
	return s.routeRaw(ctx, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install", provider), localIfEmpty(node), body)
}

// --- Upgrade ---

func (s *ProvidersService) UpgradeProvider(ctx context.Context, name, version, node string, options ...install.PlanOptions) (json.RawMessage, error) {
	ctx, cancel := ensureTimeout(ctx, constants.HTTPDownloadTimeout)
	defer cancel()

	body, _ := json.Marshal(installRequest{Version: version, PlanOptions: selectPlanOptions(options, false)})
	return s.routeRaw(ctx, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/upgrade", name), localIfEmpty(node), body)
}

// --- Uninstall ---

func (s *ProvidersService) UninstallProvider(ctx context.Context, name, node string) (json.RawMessage, error) {
	ctx, cancel := ensureTimeout(ctx, LongRequestTimeout)
	defer cancel()

	return s.routeRaw(ctx, "DELETE",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s", name), localIfEmpty(node), nil)
}

// --- Preflight ---

func (s *ProvidersService) PreflightInstall(ctx context.Context, provider, node string) (*install.PreflightReport, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()

	return routeAndParse[install.PreflightReport](ctx, s.router, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/preflight", provider), localIfEmpty(node), nil)
}

// --- Install Plan ---

func (s *ProvidersService) GetInstallPlan(ctx context.Context, provider, version, node string, options ...install.PlanOptions) (*install.Plan, error) {
	ctx, cancel := ensureTimeout(ctx, constants.HTTPDownloadTimeout)
	defer cancel()

	body, _ := json.Marshal(installRequest{Version: version, PlanOptions: selectPlanOptions(options, false)})
	return routeAndParse[install.Plan](ctx, s.router, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/plan", provider), localIfEmpty(node), body)
}

// --- Verify ---

func (s *ProvidersService) VerifyInstall(ctx context.Context, provider, version, node string, options ...install.PlanOptions) (*install.VerifyResult, error) {
	ctx, cancel := ensureTimeout(ctx, LongRequestTimeout)
	defer cancel()
	body, _ := json.Marshal(verifyRequest{Version: version, Runtime: selectPlanOptions(options, false).Runtime})
	result, err := routeAndParse[install.VerifyResult](ctx, s.router, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/verify", provider), localIfEmpty(node), body)
	if err != nil {
		return nil, err
	}
	if result.CheckContract != install.RuntimeCheckContract && result.CheckContract != "install_steps_v1" {
		result.AllOK = false
		result.Checks = append(result.Checks, install.RuntimeCheck{Name: "diagnostic_contract", Reason: "node verification contract unsupported; upgrade the node to obtain prerequisite checks"})
	}
	return result, nil
}

// --- Verify Step ---

func (s *ProvidersService) VerifyInstallStep(ctx context.Context, provider, version string, step int, node string, options ...install.PlanOptions) (*install.StepResult, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()

	body, _ := json.Marshal(verifyStepRequest{Version: version, Step: step, PlanOptions: selectPlanOptions(options, false)})
	return routeAndParse[install.StepResult](ctx, s.router, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/verify-step", provider), localIfEmpty(node), body)
}

// --- Execute Step ---

// ExecuteInstallStep is async: upstream returns 202 + {accepted,
// job_id, provider, step, node}. The raw JSON envelope is returned to
// the controller for pass-through; subscribers follow up via
// /zzrouter/v1/jobs/:id/stream for progress and the terminal StepResult.
func (s *ProvidersService) ExecuteInstallStep(ctx context.Context, provider, version string, step int, node string, options ...install.PlanOptions) (json.RawMessage, error) {
	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	body, _ := json.Marshal(verifyStepRequest{Version: version, Step: step, PlanOptions: selectPlanOptions(options, false)})
	return s.routeRaw(ctx, "POST",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/execute-step", provider), localIfEmpty(node), body)
}

// routeRaw routes a request and returns the raw JSON response body.
// Used for mutation operations (install/upgrade/uninstall) where the executor's
// response is passed through as-is. Returns a *RoutedError for 4xx/5xx so the
// controller can extract the original HTTP status code.
func (s *ProvidersService) routeRaw(ctx context.Context, method, path, node string, body []byte) (json.RawMessage, error) {
	resp, err := s.router.Route(ctx, &routing.Request{
		Path:   path,
		Method: method,
		Node:   node,
		Body:   body,
	})
	if err != nil {
		return nil, newRoutedTransportError(err, node)
	}
	if resp.StatusCode >= 400 {
		return nil, parseRoutedError(resp.StatusCode, resp.Body)
	}
	return resp.Body, nil
}

func selectPlanOptions(options []install.PlanOptions, force bool) install.PlanOptions {
	result := install.PlanOptions{Force: force}
	if len(options) > 0 {
		result = options[0]
		result.Force = result.Force || force
	}
	return result
}
