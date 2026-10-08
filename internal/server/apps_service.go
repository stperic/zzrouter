package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/routing"
)

// ============================================================================
// Apps Service - Business Logic Layer
// ============================================================================
//
// Responsibilities:
// - Read apps from the coordinator's cached EndpointRegistry (no broadcasting)
// - Filter and sort app lists
// - Route app management requests
// - No HTTP concerns (that's the handler's job)
// - No direct provider calls (that's the executor's job)

// AppInfo represents detailed information about a provider for API responses.
//
// Installed distinguishes "on disk" from "enabled at runtime", and Managed
// distinguishes "zzRouter put it there" from "it was already here". An
// installed provider with Enabled=false is dormant — present as a version file +
// manifest, not registered in the runtime protocol registry. Surfacing
// these is required for UI recovery flows: a failed install that wrote a
// partial version-file leaves a dormant entry the user must be able to
// see and uninstall, even though it isn't routable yet.
type AppInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Enabled   bool   `json:"enabled"`
	Installed bool   `json:"installed"`
	// Managed narrows Installed: zzRouter put this one here, so removing
	// it is our business. An install we merely detected is Installed
	// without being Managed, and uninstalling it would delete something
	// we do not own.
	Managed bool `json:"managed"`
	// Mode is the legacy discriminator name; Kind is the canonical
	// equivalent that matches the providers/<kind>/ directory layout
	// and the providers_controller.go catalog response. Both fields
	// carry the same value during the convergence arc; consumers
	// should prefer Kind.
	Mode        string `json:"mode"`
	Kind        string `json:"kind"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Port        int    `json:"port,omitempty"`
	Node        string `json:"node,omitempty"`
	// OS is the GOOS of the node this row describes. Carried because what a
	// node can install depends on it: a provider may resolve its upstream
	// ceiling from a different source per platform.
	OS      string `json:"os,omitempty"`
	Version string `json:"version,omitempty"`
	// LatestVersion is the newest release the provider's configured upstream
	// has published, rendered in the same shape as Version so the two can be
	// shown side by side. Served from cache only: listing providers never
	// waits on an upstream, so a cold cache leaves these empty and fills in
	// the background for the next request.
	LatestVersion string `json:"latest_version,omitempty"`
	// VersionStatus compares Version against LatestVersion: "newer" means
	// upstream is ahead, "older" means this node is, "same" means they
	// match, "unknown" means no ordering was established. Never "same" for
	// a failed or absent check.
	VersionStatus string `json:"version_status,omitempty"`
	// UpdateAvailable restates VersionStatus == "newer" without requiring
	// the reader to know which operand the comparison names. False whenever
	// the answer is unknown, never optimistically true. GET
	// /zzrouter/v1/providers/{name}/versions explains an unknown.
	UpdateAvailable  bool           `json:"update_available"`
	VersionCheckedAt string         `json:"version_checked_at,omitempty"`
	Formats          []string       `json:"formats,omitempty"`
	FormatNote       string         `json:"format_note,omitempty"`
	Capabilities     []string       `json:"capabilities,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

// AppsService handles app-related business logic
type AppsService struct {
	router   routing.Router
	provider ClusterState // Same interface as NodesService — reads from cached registry
}

// NewAppsService creates a new apps service
func NewAppsService(provider ClusterState, router routing.Router) *AppsService {
	return &AppsService{
		router:   router,
		provider: provider,
	}
}

// ListAppsRequest represents a request to list apps
type ListAppsRequest struct {
	Node    string // Target host (empty = cluster-wide)
	App     string // Filter by provider type (e.g., "ollama", "vllm")
	Name    string // Filter by provider name
	Key     string // Filter by provider key
	Running string // Filter by running status ("true"/"false")
	Format  string // Model format for app suggestion
	Refresh bool   // Force re-probe all workers before listing
}

// ListAppsResponse represents the response for listing apps
// Uses standard envelope: data, total, has_more
type ListAppsResponse struct {
	Data    []AppInfo `json:"data"`
	Total   int       `json:"total"`
	HasMore bool      `json:"has_more,omitempty"`
}

// ListApps retrieves all apps by reading from the coordinator's cached state.
// No broadcasting — apps data is collected during health checks.
// Pass Refresh=true to synchronously re-probe all workers before reading.
func (s *AppsService) ListApps(_ context.Context, req *ListAppsRequest) (*ListAppsResponse, error) {
	slog.Info("[AppsService] ListApps", "node", req.Node, "provider", req.App, "name", req.Name, "refresh", req.Refresh)

	if s.provider == nil {
		return &ListAppsResponse{}, nil
	}

	// If refresh requested, re-probe all workers to get fresh data
	if req.Refresh {
		s.provider.RefreshClusterEndpoints()
	}

	// Local and remote stay separate so the node the caller is on sorts
	// first, ahead of the workers, rather than wherever its name falls.
	var localApps, remoteApps []AppInfo

	// 1. Add local apps
	for _, app := range s.provider.GetLocalAppsInfo() {
		info := appInfoFromLocal(app, app.Node)
		info.OS = runtime.GOOS
		if matchesAppFilters(info, req) {
			localApps = append(localApps, info)
		}
	}

	// 2. Add worker apps from EndpointRegistry (coordinator only)
	endpoints := s.provider.GetClusterEndpoints()
	for _, ep := range endpoints {
		if ep.IsLocal {
			continue
		}
		for _, app := range ep.Snapshot.Apps {
			node := ep.NodeName
			if node == "" {
				node = app.Node
			}
			info := appInfoFromLocal(app, node)
			info.OS = goosFromNodeOS(ep.Snapshot.OS)
			if matchesAppFilters(info, req) {
				remoteApps = append(remoteApps, info)
			}
		}
	}

	sortApps(localApps)
	sortApps(remoteApps)
	allApps := append(localApps, remoteApps...) //nolint:gocritic // two ordered groups, local first

	slog.Info("[AppsService] Returning provider apps", "count", len(allApps))
	return &ListAppsResponse{
		Data:    allApps,
		Total:   len(allApps),
		HasMore: false,
	}, nil
}

// goosFromNodeOS maps a node's advertised OS name to a GOOS value.
//
// Nodes advertise a display name ("macOS", "Linux") via getOSName, while
// platform overrides key on GOOS. Translating here keeps the health payload
// readable and keeps override matching exact. An unrecognised name yields "",
// which resolves to the base source rather than to a guessed platform:
// comparing against the wrong ceiling is worse than comparing against the
// project's own release.
func goosFromNodeOS(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "macos", "darwin":
		return "darwin"
	case "linux":
		return "linux"
	case "windows":
		return "windows"
	default:
		return ""
	}
}

// sortApps orders providers by node, then by name.
//
// Both sources underneath are maps — the endpoint registry is keyed by URL
// and the provider manager by name — so every call returned a different
// order. That is not cosmetic. The TUI acts on whatever row the cursor sits
// on, so a refresh landing between "select llamacpp" and "press U" sends the
// upgrade to whichever provider took that row, and the one the user was
// looking at appears to do nothing.

func sortApps(apps []AppInfo) {
	// Stable, so two rows sharing a node and a name keep a fixed order
	// rather than falling back to map order — which is the reshuffle this
	// sort exists to remove.
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Node != apps[j].Node {
			return apps[i].Node < apps[j].Node
		}
		return apps[i].Name < apps[j].Name
	})
}

// appInfoFromLocal converts a typed LocalProviderInfo into the response
// DTO. node overrides the embedded Node (coord rewrites with endpoint
// name); pass info.Node to keep it.
func appInfoFromLocal(info prov_apps.LocalProviderInfo, node string) AppInfo {
	return AppInfo{
		Name:        info.Name,
		Type:        info.Type,
		Node:        node,
		Description: info.Description,
		Version:     info.Version,
		Mode:        info.Mode,
		Kind:        info.Kind,
		Endpoint:    info.Endpoint,
		FormatNote:  info.FormatNote,
		Formats:     info.Formats,
		Enabled:     info.State == prov_apps.StateLive || info.State == prov_apps.StateCloudAvailable,
		Installed:   info.State.IsInstalled(),
		Managed:     info.Managed,
	}
}

// matchesAppFilters checks if an app matches the request filters
func matchesAppFilters(info AppInfo, req *ListAppsRequest) bool {
	if req.Node != "" && !strings.EqualFold(info.Node, req.Node) {
		return false
	}
	if req.App != "" && !strings.EqualFold(info.Type, req.App) {
		return false
	}
	if req.Name != "" && !strings.EqualFold(info.Name, req.Name) {
		return false
	}
	if req.Running != "" && req.Running != "true" {
		return false
	}
	return true
}

// GetAppRequest represents a request to get a specific app
type GetAppRequest struct {
	Name string // App name/key
	Node string // Target host (optional)
}

// GetAppResponse represents the response for getting an app
type GetAppResponse struct {
	App AppInfo `json:"provider"`
}

// GetApp retrieves details for a specific app
func (s *AppsService) GetApp(ctx context.Context, req *GetAppRequest) (*GetAppResponse, error) {
	slog.Info("[AppsService] GetApp", "name", req.Name, "node", req.Node)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	app, err := routeAndParse[AppInfo](ctx, s.router, "GET",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s", req.Name), req.Node, nil)
	if err != nil {
		return nil, err
	}

	if app.Name == "" {
		return nil, fmt.Errorf(constants.ErrProviderNotFoundFmt, req.Name)
	}

	slog.Info("[AppsService] Returning app", "provider", req.Name)
	return &GetAppResponse{App: *app}, nil
}

// UpdateAppRequest represents a request to update an app
type UpdateAppRequest struct {
	Name    string // App name/key
	Node    string // Target host (optional)
	Enabled *bool  `json:"enabled,omitempty"` // Enable/disable app
	// PinnedVersion sets the install version pin. Pointer-typed so the
	// three JSON states stay distinct: absent leaves the pin alone,
	// "" clears it (resolve latest), a value pins that release.
	PinnedVersion *string `json:"pinned_version,omitempty"`
}

// UpdateAppResponse represents the response from updating an app
type UpdateAppResponse struct {
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	PinnedVersion string `json:"pinned_version,omitempty"`
	Message       string `json:"message"`
}

// UpdateApp updates an app's configuration
func (s *AppsService) UpdateApp(ctx context.Context, req *UpdateAppRequest) (*UpdateAppResponse, error) {
	slog.Info("[AppsService] UpdateApp", "name", req.Name, "enabled", req.Enabled)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Create routing request
	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	updateResp, err := routeAndParse[UpdateAppResponse](ctx, s.router, "PATCH",
		fmt.Sprintf("/zzrouter/v1/internal/providers/%s", req.Name), req.Node, requestBody)
	if err != nil {
		return nil, err
	}

	slog.Info("[AppsService] App  updated successfully", "name", req.Name)
	return updateResp, nil
}
