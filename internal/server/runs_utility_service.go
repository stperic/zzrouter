package server

import (
	"context"
	"fmt"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/port"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// RunsUtilityService provides local utility/query operations for run
// management: capabilities, parameters, metrics, config, ports, and
// validation. These are coordinator-local queries that don't need
// cluster routing.
type RunsUtilityService struct {
	appMgr     *prov_apps.ProviderAppManager
	appsConfig func() *pkgConfig.AppsConfig
}

// NewRunsUtilityService creates a new runs utility service.
func NewRunsUtilityService(appMgr *prov_apps.ProviderAppManager, appsConfig func() *pkgConfig.AppsConfig) *RunsUtilityService {
	return &RunsUtilityService{appMgr: appMgr, appsConfig: appsConfig}
}

// ListCapabilities returns all providers that can be run.
func (svc *RunsUtilityService) ListCapabilities() (map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	appsCfg := svc.appsConfig()
	var apps []map[string]any
	appsCfg.RangeApps(func(name string, cfg pkgConfig.ServiceConfig) bool {
		apps = append(apps, map[string]any{
			"type":     name,
			"mode":     cfg.Mode,
			"enabled":  cfg.IsEnabled(),
			"protocol": string(cfg.Protocol),
		})
		return true
	})
	return map[string]any{"providers": apps}, nil
}

// GetAppCapabilities returns detailed capabilities for a specific provider.
func (svc *RunsUtilityService) GetAppCapabilities(providerType string) (map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	appsCfg := svc.appsConfig()
	cfg, exists := appsCfg.LookupApp(providerType)
	if !exists {
		return nil, fmt.Errorf("provider type %q not found", providerType)
	}

	result := map[string]any{
		"type":     providerType,
		"mode":     cfg.Mode,
		"enabled":  cfg.IsEnabled(),
		"protocol": string(cfg.Protocol),
	}
	if cfg.Runtime != nil {
		result["endpoint"] = cfg.Runtime.Endpoint
		if cfg.Runtime.Execution.Command != "" {
			result["default_command"] = cfg.Runtime.Execution.Command
			result["default_args"] = cfg.Runtime.Execution.Args
		}
	}
	return result, nil
}

// GetAppParameters returns the parameter schema for a provider.
func (svc *RunsUtilityService) GetAppParameters(providerType string) (*AppParametersResponse, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	cfg, err := getAppConfig(svc.appsConfig(), providerType)
	if err != nil {
		return nil, err
	}

	resolved := cfg.Resolve("", "")
	return buildLegacyAppResponse(providerType, resolved), nil
}

// ValidateFiles validates file mount configuration.
func (svc *RunsUtilityService) ValidateFiles(files []instance.FileMount) map[string]any {
	processor := process.NewFileMountProcessor()
	results := make([]map[string]any, 0, len(files))
	var warnings, errors []string
	allValid := true

	for _, file := range files {
		result := map[string]any{"name": file.Name}

		if err := processor.ValidateFileMount(file); err != nil {
			result["status"] = "invalid"
			result["error"] = err.Error()
			errors = append(errors, fmt.Sprintf("%s: %s", file.Name, err.Error()))
			allValid = false
		} else {
			result["status"] = "valid"
			if !file.IsOutput {
				result["exists"] = true
				result["readable"] = true
			} else {
				result["parent_dir_exists"] = true
				result["parent_dir_writable"] = true
			}
		}
		results = append(results, result)
	}

	return map[string]any{
		"valid":    allValid,
		"files":    results,
		"warnings": warnings,
		"errors":   errors,
	}
}

// GetFileMountExamples returns example file mount configurations.
func (svc *RunsUtilityService) GetFileMountExamples(provider, useCase string) map[string]any {
	examples := []map[string]any{
		{
			"name":        "config.json",
			"source":      "/path/to/config.json",
			"destination": "/app/config.json",
			"is_output":   false,
			"description": "Mount a configuration file",
		},
		{
			"name":        "output.log",
			"source":      "/path/to/output.log",
			"destination": "/app/logs/output.log",
			"is_output":   true,
			"description": "Mount an output file (will be created)",
		},
	}
	return map[string]any{
		"examples": examples,
		"provider": provider,
		"use_case": useCase,
	}
}

// GetMetrics returns system-wide run metrics.
func (svc *RunsUtilityService) GetMetrics() (map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	providerMetrics := make(map[string]map[string]any)
	for _, providerName := range svc.appMgr.SupportedProviders() {
		instances := svc.appMgr.Instances().ListByProvider(providerName)
		running := 0
		for _, inst := range instances {
			if inst.GetStatus() == instance.StatusRunning {
				running++
			}
		}
		providerMetrics[providerName] = map[string]any{
			"total_launched":    len(instances),
			"currently_running": running,
		}
	}

	stats := svc.appMgr.Instances().Stats()
	instanceCounts, _ := stats["instances"].(map[string]int)

	return map[string]any{
		"instances_launched": instanceCounts["total"],
		"instances_running":  instanceCounts["running"],
		"instances_failed":   instanceCounts["failed"],
		"ports_allocated":    len(svc.appMgr.Ports().ListAllAllocated()),
		"provider_metrics":   providerMetrics,
	}, nil
}

// GetConfig returns the current runs configuration.
func (svc *RunsUtilityService) GetConfig() (map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	poolStats := svc.appMgr.Ports().GetPoolStats()
	start, end := aggregatePortRange(poolStats)
	hcConfig := svc.appMgr.HealthCheckConfig()

	return map[string]any{
		"port_range": map[string]any{"start": start, "end": end},
		"health_check": map[string]any{
			"interval":      hcConfig.Interval.String(),
			"timeout":       hcConfig.Timeout.String(),
			"max_retries":   hcConfig.MaxRetries,
			"startup_grace": hcConfig.StartupGrace.String(),
		},
	}, nil
}

// ListAllocatedPorts returns all currently allocated ports.
func (svc *RunsUtilityService) ListAllocatedPorts() (map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	poolStats := svc.appMgr.Ports().GetPoolStats()
	start, end := aggregatePortRange(poolStats)

	var allocated []int
	for _, ps := range poolStats {
		allocated = append(allocated, ps.Ports...)
	}

	available := 0
	if end > start {
		available = end - start + 1 - len(allocated)
	}

	return map[string]any{
		"allocated_ports": allocated,
		"available_ports": available,
		"port_range":      map[string]any{"start": start, "end": end},
	}, nil
}

// BatchLaunch launches multiple instances. Returns launched and failed lists.
func (svc *RunsUtilityService) BatchLaunch(ctx context.Context, instances []LaunchRequest) ([]map[string]any, []map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, nil, err
	}

	var launched, failed []map[string]any

	for _, req := range instances {
		if err := validateParameterMaps(req.Parameters, req.EnvVars); err != nil {
			failed = append(failed, map[string]any{
				"provider": req.Provider,
				"error":    fmt.Sprintf("invalid parameters: %v", err),
			})
			continue
		}

		if _, err := parseLaunchMode(req.LaunchMode); err != nil {
			failed = append(failed, map[string]any{"provider": req.Provider, "error": err.Error()})
			continue
		}

		provReq := buildLaunchRequest(req.Provider, req)
		inst, err := svc.appMgr.LaunchInstance(ctx, provReq)
		if err != nil {
			failed = append(failed, map[string]any{"provider": req.Provider, "error": err.Error()})
			continue
		}

		launched = append(launched, map[string]any{
			"id":       inst.ID,
			"provider": inst.Provider,
			"port":     inst.Port,
		})
	}
	return launched, failed, nil
}

// ValidateConfig validates instance configuration.
func (svc *RunsUtilityService) ValidateConfig(provider, launchMode string) (map[string]any, error) {
	if err := svc.requireAppMgr(); err != nil {
		return nil, err
	}

	if !svc.appMgr.IsProviderSupported(provider) {
		return nil, fmt.Errorf("unsupported provider type: %s", provider)
	}

	if _, err := parseLaunchMode(launchMode); err != nil {
		return nil, err
	}

	return map[string]any{
		"valid":   true,
		"message": "Configuration is valid",
	}, nil
}

// requireAppMgr returns an error if the provider app manager is not available.
func (svc *RunsUtilityService) requireAppMgr() error {
	if svc.appMgr == nil {
		return errProviderMgrUnavailable
	}
	return nil
}

var errProviderMgrUnavailable = fmt.Errorf("provider manager not initialized: on-demand provider management is not available")

// aggregatePortRange returns the min start and max end across all provider port pools.
func aggregatePortRange(poolStats map[string]port.PoolStats) (start, end int) {
	for _, ps := range poolStats {
		if start == 0 || ps.RangeStart < start {
			start = ps.RangeStart
		}
		if ps.RangeEnd > end {
			end = ps.RangeEnd
		}
	}
	return
}
