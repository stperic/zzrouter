package config

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/constants"
)

// serviceConfigToProvider synthesizes a typed Provider from a legacy
// ServiceConfig. Dispatches on ServiceConfig.Mode — the legacy `service`
// and `external` modes both map to *ExternalProvider (the collapsed kind
// in the new shape).
//
// Called from AddApp and UpdateApp at every mutation boundary — converts
// the user-facing ServiceConfig view into the typed Provider that
// c.providers stores.
func serviceConfigToProvider(name string, sc ServiceConfig) (Provider, error) {
	switch sc.Mode {
	case constants.AppModeOnDemand:
		return &OnDemandProvider{
			Install:         sc.Install,
			Features:        sc.Features,
			Name:            name,
			Description:     sc.Description,
			Protocol:        sc.Protocol,
			Enabled:         sc.Enabled,
			PinnedVersion:   sc.PinnedVersion,
			VersionSource:   sc.VersionSource,
			Platforms:       sc.Platforms,
			Requirements:    sc.Requirements,
			InstallVariants: sc.InstallVariants,
			Capabilities:    sc.Capabilities,
			Discovery:       sc.Discovery,
			Defaults:        sc.Defaults,
			Models:          sc.Models,
			Nodes:           sc.Nodes,
			Search:          sc.Search,
			Runtime:         runtimeToOnDemand(sc.Runtime),
		}, nil

	case constants.AppModeExternal, constants.AppModeService:
		// Legacy shape has two distinct modes (external = remote daemon,
		// service = local daemon zzRouter can restart); both map to
		// ExternalProvider in the new shape. The service-manager config
		// stays on the typed struct so systemd/launchd/nssm paths keep
		// working for ollama.
		return &ExternalProvider{
			Features:        sc.Features,
			Name:            name,
			Description:     sc.Description,
			Protocol:        sc.Protocol,
			Enabled:         sc.Enabled,
			PinnedVersion:   sc.PinnedVersion,
			VersionSource:   sc.VersionSource,
			Requirements:    sc.Requirements,
			InstallVariants: sc.InstallVariants,
			Capabilities:    sc.Capabilities,
			Discovery:       sc.Discovery,
			Defaults:        sc.Defaults,
			Models:          sc.Models,
			Nodes:           sc.Nodes,
			Service:         sc.Service,
			Search:          sc.Search,
			Runtime:         runtimeToExternal(sc.Runtime),
		}, nil

	case constants.AppModeCloud:
		return &CloudProvider{
			Features:     sc.Features,
			Name:         name,
			Description:  sc.Description,
			Enabled:      sc.Enabled,
			Capabilities: sc.Capabilities,
			Search:       sc.Search,
			Runtime:      runtimeToCloud(sc.Runtime),
		}, nil

	case constants.AppModeRegistry:
		return &SearchRegistry{
			Name:        name,
			Description: sc.Description,
			Enabled:     sc.Enabled,
			Search:      sc.Search,
			Runtime:     runtimeToRegistry(sc.Runtime),
		}, nil

	default:
		return nil, fmt.Errorf("provider '%s': unknown mode '%s'", name, sc.Mode)
	}
}

func runtimeToOnDemand(rt *AppRuntimeConfig) OnDemandRuntime {
	if rt == nil {
		return OnDemandRuntime{}
	}
	return OnDemandRuntime{
		PortRange:             rt.PortRange,
		BasePort:              rt.BasePort,
		KeepAlive:             rt.KeepAlive,
		MaxConcurrentRequests: rt.MaxConcurrentRequests,
		QueueTimeout:          rt.QueueTimeout,
		Execution:             rt.Execution,
		HealthCheck:           rt.HealthCheck,
		Container:             rt.Container,
		Resources:             rt.Resources,
	}
}

func runtimeToExternal(rt *AppRuntimeConfig) ExternalRuntime {
	if rt == nil {
		return ExternalRuntime{}
	}
	return ExternalRuntime{
		Endpoint:    rt.Endpoint,
		ModelURL:    rt.ModelURLFmt,
		KeepAlive:   rt.KeepAlive,
		API:         rt.API,
		HealthCheck: rt.HealthCheck,
	}
}

func runtimeToCloud(rt *AppRuntimeConfig) CloudRuntime {
	if rt == nil {
		return CloudRuntime{}
	}
	out := CloudRuntime{
		Endpoint:          rt.Endpoint,
		ModelURL:          rt.ModelURLFmt,
		ModelsPath:        rt.ModelsPath,
		ModelsResponseKey: rt.ModelsResponseKey,
		HealthCheck:       rt.HealthCheck,
	}
	if rt.API != nil {
		out.API = *rt.API
	}
	return out
}

func runtimeToRegistry(rt *AppRuntimeConfig) RegistryRuntime {
	if rt == nil {
		return RegistryRuntime{}
	}
	return RegistryRuntime{
		Endpoint: rt.Endpoint,
		ModelURL: rt.ModelURLFmt,
	}
}
