package shared

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	pkgUtils "github.com/stperic/zzrouter/pkg/utils"
)

// FetchModels retrieves and sorts models from the registry.
func FetchModels(client *pkgClient.Client, params *ModelQueryParams, refresh bool) ([]pkgClient.ModelMetadata, error) {
	models, err := client.ListModels(pkgClient.ModelFilter{
		Node: params.Node, Registry: params.Registry, Provider: params.Provider, Model: params.Model, Refresh: refresh,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query models: %w", err)
	}
	SortModelRows(models, params.SortBy)
	return models, nil
}

// FetchInstances retrieves and filters running instances.
func FetchInstances(client *pkgClient.Client, app, model string) ([]pkgClient.Instance, error) {
	instances, err := client.ListInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	return FilterInstances(instances, app, model), nil
}

// FetchNodes retrieves and parses cluster node information.
func FetchNodes(client *pkgClient.Client, fields string, refresh bool) ([]NodeInfo, error) {
	response, err := client.GetNodesWithRefresh(fields, refresh)
	if err != nil {
		return nil, fmt.Errorf("failed to get nodes: %w", err)
	}
	return ParseNodesFromResponse(response)
}

// FetchDeployments retrieves and parses deployment status from the server.
func FetchDeployments(client *pkgClient.Client) ([]DeploymentInfo, error) {
	deployments, err := client.ListDeployments()
	if err != nil {
		return nil, fmt.Errorf("failed to get deploy status: %w", err)
	}

	var rows []DeploymentInfo
	for _, d := range deployments {
		for _, n := range d.Nodes {
			statusStr := n.Status
			if n.Error != "" {
				statusStr = constants.StatusFailed
			}

			progressStr := EmptyValue
			if n.Progress > 0 {
				progressStr = fmt.Sprintf("%.1f%%", n.Progress)
			}

			sizeStr := EmptyValue
			if n.BytesTotal > 0 {
				sizeStr = FormatSize(n.BytesTotal)
			}

			speedStr := EmptyValue
			if n.Speed > 0 {
				speedStr = FormatSpeed(n.Speed)
			}

			etaStr := EmptyValue
			if statusStr == constants.StatusDownloading && n.BytesTotal > 0 && n.Speed > 0 {
				remaining := n.BytesTotal - n.BytesDownloaded
				if remaining > 0 {
					etaStr = FormatDuration(time.Duration(remaining/n.Speed)*time.Second, true)
				}
			}

			provider := d.Registry
			if provider == "" {
				parts := strings.SplitN(n.DownloadID, "/", 3)
				if len(parts) >= 2 {
					provider = parts[1]
				} else {
					provider = "unknown"
				}
			}

			rows = append(rows, DeploymentInfo{
				DownloadID:   n.DownloadID,
				DeploymentID: d.ID,
				JobID:        n.JobID,
				Node:         n.Node,
				Model:        d.Model,
				Provider:     provider,
				Status:       statusStr,
				Progress:     progressStr,
				Size:         sizeStr,
				Speed:        speedStr,
				ETA:          etaStr,
				Error:        n.Error,
			})
		}
	}

	return rows, nil
}

// FetchProviders retrieves, parses, and optionally filters provider apps.
func FetchProviders(client *pkgClient.Client, node, providerFilter string, refresh ...bool) ([]ProviderInfo, error) {
	response, err := client.GetProviders(node, refresh...)
	if err != nil {
		return nil, fmt.Errorf("failed to get providers: %w", err)
	}
	apps, err := ParseProviders(response)
	if err != nil {
		return nil, err
	}
	if providerFilter != "" {
		apps = FilterProviders(apps, providerFilter)
	}
	return apps, nil
}

// =============================================================================
// PARSING HELPERS
// =============================================================================

// ParseNodesFromResponse parses the API response into NodeInfo structs.
func ParseNodesFromResponse(response map[string]any) ([]NodeInfo, error) {
	var nodes []NodeInfo

	dataVal, exists := response["data"]
	if !exists {
		return nil, fmt.Errorf("invalid response format: missing 'data' field")
	}
	hostsData, ok := dataVal.([]any)
	if !ok {
		return []NodeInfo{}, nil
	}

	for _, h := range hostsData {
		hostMap, ok := h.(map[string]any)
		if !ok {
			continue
		}

		node := NodeInfo{
			Name:          GetStrField(hostMap, "name"),
			IPAddress:     GetStrField(hostMap, "ip_address"),
			ClusterRole:   GetStrField(hostMap, "cluster_role"),
			HealthStatus:  GetStrField(hostMap, "health_status"),
			OS:            GetStrField(hostMap, "os"),
			Version:       GetStrField(hostMap, "version"),
			Disk:          FormatDisk(hostMap),
			Memory:        FormatMemory(hostMap),
			GPU:           FormatGPU(hostMap),
			GPUDesc:       FormatGPUDesc(hostMap),
			GPUs:          ParseGPUs(hostMap),
			UptimeSeconds: GetIntField(hostMap, "uptime_seconds"),
			Address:       GetStrField(hostMap, "address"),
			Providers:     ParseNodeProviders(hostMap),
			LastError:     GetStrField(hostMap, "last_error"),
		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

// ParseProviders parses the API response into ProviderInfo structs.
func ParseProviders(response map[string]any) ([]ProviderInfo, error) {
	var providers []ProviderInfo

	appsData, ok := response["data"].([]any)
	if !ok {
		appsData, ok = response["providers"].([]any)
		if !ok {
			return nil, nil
		}
	}

	for _, p := range appsData {
		providerMap, ok := p.(map[string]any)
		if !ok {
			continue
		}

		provider := ProviderInfo{
			Name:       GetStrField(providerMap, "name"),
			Node:       GetStrField(providerMap, "node"),
			Enabled:    GetBoolFieldValue(providerMap, "enabled"),
			Version:    GetStrField(providerMap, "version"),
			Mode:       GetStrField(providerMap, "mode"),
			Kind:       GetStrField(providerMap, "kind"),
			Formats:    GetStrArrayField(providerMap, "formats"),
			FormatNote: GetStrField(providerMap, "format_note"),
			// Upstream release fields. This mapping is manual, so a field
			// added to ProviderInfo and to the API is still dropped here
			// until it is listed: the struct tag does no work on this path.
			LatestVersion:    GetStrField(providerMap, "latest_version"),
			VersionStatus:    GetStrField(providerMap, "version_status"),
			VersionCheckedAt: GetStrField(providerMap, "version_checked_at"),
		}

		providers = append(providers, provider)
	}

	return providers, nil
}

// FilterProviders filters providers by name pattern.
func FilterProviders(providers []ProviderInfo, pattern string) []ProviderInfo {
	if pattern == "" {
		return providers
	}

	pattern = strings.ToLower(strings.TrimSuffix(pattern, "*"))

	var filtered []ProviderInfo
	for _, provider := range providers {
		if strings.Contains(strings.ToLower(provider.Name), pattern) {
			filtered = append(filtered, provider)
		}
	}

	return filtered
}

// SortModelRows sorts model metadata by the given field.
func SortModelRows(models []pkgClient.ModelMetadata, sortBy string) {
	switch strings.ToLower(sortBy) {
	case "node":
		sort.Slice(models, func(i, j int) bool {
			return strings.ToLower(models[i].Node) < strings.ToLower(models[j].Node)
		})
	case "registry":
		sort.Slice(models, func(i, j int) bool {
			return strings.ToLower(models[i].SourceRepo) < strings.ToLower(models[j].SourceRepo)
		})
	case "model", "name":
		sort.Slice(models, func(i, j int) bool {
			return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
		})
	case "size":
		sort.Slice(models, func(i, j int) bool {
			return models[i].Size > models[j].Size
		})
	case "date", "modified":
		sort.Slice(models, func(i, j int) bool {
			return models[i].GetModifiedTime().After(models[j].GetModifiedTime())
		})
	default:
		sort.Slice(models, func(i, j int) bool {
			return models[i].GetModifiedTime().After(models[j].GetModifiedTime())
		})
	}
}

// FilterInstances filters instances by app and model patterns.
func FilterInstances(instances []pkgClient.Instance, app, model string) []pkgClient.Instance {
	if app == "" && model == "" {
		return instances
	}

	var filtered []pkgClient.Instance
	for _, inst := range instances {
		if app != "" {
			providerPattern := fmt.Sprintf("/%s/%s", inst.Node, inst.App)
			matched := pkgUtils.MatchPattern(app, providerPattern)
			if !matched {
				continue
			}
		}

		if model != "" {
			matched := pkgUtils.MatchPattern(model, inst.Model)
			if !matched {
				continue
			}
		}

		filtered = append(filtered, inst)
	}

	return filtered
}

// ParseNodeProviders extracts provider info from a node's host map.
func ParseNodeProviders(hostMap map[string]any) []NodeProviderInfo {
	providersRaw, ok := hostMap["providers"].([]any)
	if !ok {
		return nil
	}
	var providers []NodeProviderInfo
	for _, a := range providersRaw {
		provMap, ok := a.(map[string]any)
		if !ok {
			continue
		}
		formats := ""
		if fmts, ok := provMap["formats"].([]any); ok {
			parts := make([]string, 0, len(fmts))
			for _, f := range fmts {
				if s, ok := f.(string); ok {
					parts = append(parts, s)
				}
			}
			formats = strings.Join(parts, ", ")
		}
		providers = append(providers, NodeProviderInfo{
			Key:     GetStrField(provMap, "key"),
			Name:    GetStrField(provMap, "name"),
			Type:    GetStrField(provMap, "type"),
			Formats: formats,
		})
	}
	return providers
}

// ParseGPUs extracts individual GPU details from a node's data.
func ParseGPUs(hostMap map[string]any) []GPUInfo {
	gpuData, ok := hostMap["gpu"].(map[string]any)
	if !ok {
		return nil
	}
	gpus, ok := gpuData["gpus"].([]any)
	if !ok {
		return nil
	}
	var result []GPUInfo
	for _, g := range gpus {
		gpu, ok := g.(map[string]any)
		if !ok {
			continue
		}
		idx, _ := gpu["index"].(float64)
		totalGB, _ := gpu["memory_total_gb"].(float64)
		var availGB *float64
		if v, present := gpu["memory_available_gb"].(float64); present {
			availGB = &v
		}
		name, _ := gpu["name"].(string)
		vendor, _ := gpu["vendor"].(string)
		pciAddr, _ := gpu["pci_address"].(string)
		uuid, _ := gpu["uuid"].(string)
		var driverIdx *int
		if v, present := gpu["driver_index"].(float64); present {
			n := int(v)
			driverIdx = &n
		}
		result = append(result, GPUInfo{
			Index:       int(idx),
			Name:        name,
			Vendor:      vendor,
			PCIAddress:  pciAddr,
			UUID:        uuid,
			DriverIndex: driverIdx,
			VRAMTotalGB: totalGB,
			VRAMFreeGB:  availGB,
		})
	}
	return result
}

// =============================================================================
// FIELD HELPERS
// =============================================================================

func GetStrField(m map[string]any, key string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

func GetIntField(m map[string]any, key string) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

func GetBoolFieldValue(m map[string]any, key string) bool {
	if val, ok := m[key]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return false
}

func GetStrArrayField(m map[string]any, key string) []string {
	if val, ok := m[key]; ok {
		if arr, ok := val.([]any); ok {
			result := make([]string, 0, len(arr))
			for _, item := range arr {
				if str, ok := item.(string); ok {
					result = append(result, str)
				}
			}
			return result
		}
	}
	return nil
}

// FormatDisk formats disk usage from a host map.
func FormatDisk(hostMap map[string]any) string {
	diskData, ok := hostMap["disk"].(map[string]any)
	if !ok {
		return "-"
	}

	totalGB, _ := diskData["total_gb"].(float64)
	availableGB, _ := diskData["available_gb"].(float64)

	if totalGB == 0 {
		return "-"
	}

	return fmt.Sprintf("%.0f / %.0f GB", availableGB, totalGB)
}

// FormatMemory formats memory usage from a host map.
func FormatMemory(hostMap map[string]any) string {
	memData, ok := hostMap["memory"].(map[string]any)
	if !ok {
		return "-"
	}

	totalGB, _ := memData["total_gb"].(float64)
	availableGB, _ := memData["available_gb"].(float64)

	if totalGB == 0 {
		return "-"
	}

	return fmt.Sprintf("%.0f / %.0f GB", availableGB, totalGB)
}

// FormatGPU formats GPU VRAM summary from a host map.
func FormatGPU(hostMap map[string]any) string {
	gpuData, ok := hostMap["gpu"].(map[string]any)
	if !ok {
		return "-"
	}

	count, _ := gpuData["count"].(float64)
	gpuType, _ := gpuData["type"].(string)

	if count == 0 || gpuType == "none" {
		return "-"
	}

	gpus, ok := gpuData["gpus"].([]any)
	if !ok || len(gpus) == 0 {
		return "-"
	}

	var totalVRAM float64
	var availableVRAM float64

	for _, g := range gpus {
		gpu, ok := g.(map[string]any)
		if !ok {
			continue
		}
		memoryTotal, _ := gpu["memory_total_gb"].(float64)
		memoryAvailable, _ := gpu["memory_available_gb"].(float64)

		totalVRAM += memoryTotal
		availableVRAM += memoryAvailable
	}

	if totalVRAM == 0 {
		return "-"
	}

	gpuCount := int(count)
	if availableVRAM > 0 {
		// Labelled even in the narrow list column: an unlabelled
		// "65 / 96 GB" reads as used-of-total just as easily as free.
		return fmt.Sprintf("%.0f free/%.0f GB [%d]", availableVRAM, totalVRAM, gpuCount)
	}

	return fmt.Sprintf("%.0f GB [%d]", totalVRAM, gpuCount)
}

// FormatGPUDesc formats GPU description from a host map.
func FormatGPUDesc(hostMap map[string]any) string {
	gpuData, ok := hostMap["gpu"].(map[string]any)
	if !ok {
		return "-"
	}

	count, _ := gpuData["count"].(float64)
	gpuType, _ := gpuData["type"].(string)

	if count == 0 || gpuType == "none" {
		return "-"
	}

	gpus, ok := gpuData["gpus"].([]any)
	if ok && len(gpus) > 0 {
		if gpu, ok := gpus[0].(map[string]any); ok {
			if name, ok := gpu["name"].(string); ok && name != "" {
				var result string
				if count == 1 {
					result = fmt.Sprintf("1x %s", name)
				} else {
					result = fmt.Sprintf("%.0fx %s", count, name)
				}

				if len(result) > 25 {
					return result[:22] + "..."
				}
				return result
			}
		}
	}

	result := ""
	if count == 1 {
		result = fmt.Sprintf("1x %s", gpuType)
	} else {
		result = fmt.Sprintf("%.0fx %s", count, gpuType)
	}

	if len(result) > 25 {
		return result[:22] + "..."
	}
	return result
}

// jobsFanOutTimeout bounds each per-node jobs query. Well under the
// client's general timeout so one unreachable peer costs a short pause
// rather than the whole request budget.
const jobsFanOutTimeout = 5 * time.Second

// isWorkJob reports whether a job kind is a unit of work with an end,
// as opposed to a continuous stream. The registry also holds firehose
// jobs — inference_log runs one permanent subscription per node — which
// would otherwise sit in an activity view forever as un-finishable rows.
//
// Exclusion rather than an allowlist, so a newly added finite kind shows
// up on its own instead of being silently dropped.
func isWorkJob(kind string) bool {
	return kind != string(jobs.KindInferenceLog)
}

// FetchAllJobs returns every in-flight job across the cluster.
//
// The server's /jobs endpoint reports only the registry of the node that
// serves it, so a cluster-wide view has to fan out: query the local node,
// then each peer by name. Node failures are skipped rather than fatal —
// one unreachable worker must not blank the whole activity view, which is
// exactly when an operator is most likely to be looking at it.
func FetchAllJobs(ctx context.Context, client *pkgClient.Client) ([]pkgClient.JobEvent, error) {
	nodes, err := FetchNodes(client, "", false)
	if err != nil {
		// No node list is survivable: report what the local node knows,
		// filtered the same way the fan-out filters.
		local, lerr := client.ListJobs(ctx, "")
		if lerr != nil {
			return nil, lerr
		}
		return filterWorkJobs(local), nil
	}

	// Query peers concurrently under a short deadline. Serially, a node
	// that is powered off but still reported healthy costs the client's
	// full request timeout before the next is tried, so one dead worker
	// stalls the whole view — precisely when an operator needs it.
	targets := []string{""} // "" is the node this client points at
	for _, n := range nodes {
		if n.Name == "" || strings.HasPrefix(n.HealthStatus, "down") {
			continue
		}
		targets = append(targets, n.Name)
	}

	results := make([][]pkgClient.JobEvent, len(targets))
	var wg sync.WaitGroup
	for i, node := range targets {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			nodeCtx, cancel := context.WithTimeout(ctx, jobsFanOutTimeout)
			defer cancel()
			jobs, jerr := client.ListJobs(nodeCtx, node)
			if jerr != nil {
				return // an unreachable peer must not blank the view
			}
			results[i] = jobs
		}(i, node)
	}
	wg.Wait()

	// Fixed target order keeps rows stable across refreshes, and the local
	// node is first so its copy wins the dedup if a peer echoes it.
	seen := map[string]bool{}
	var out []pkgClient.JobEvent
	for _, jobs := range results {
		for _, j := range jobs {
			if j.JobID == "" || seen[j.JobID] || !isWorkJob(j.Kind) {
				continue
			}
			seen[j.JobID] = true
			out = append(out, j)
		}
	}
	return out, nil
}

// filterWorkJobs drops firehose kinds and unaddressable entries.
func filterWorkJobs(in []pkgClient.JobEvent) []pkgClient.JobEvent {
	var out []pkgClient.JobEvent
	for _, j := range in {
		if j.JobID == "" || !isWorkJob(j.Kind) {
			continue
		}
		out = append(out, j)
	}
	return out
}

// FormatPayloadMark renders whether an inference log entry's request
// bodies are still retained and fetchable.
func FormatPayloadMark(available bool) string {
	if available {
		return "yes"
	}
	return "-"
}
