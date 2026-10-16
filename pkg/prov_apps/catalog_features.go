package prov_apps

import (
	"maps"
	"slices"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// ModelFeatureDetails reports node-local feature evidence and effective endpoints.
// Missing evidence stays unknown rather than guessing from the model name.
func (m *ProviderAppManager) ModelFeatureDetails(provider, model string, details map[string]any) map[string]any {
	out := maps.Clone(details)
	if out == nil {
		out = map[string]any{}
	}
	svc, ok := m.ProviderConfig(provider)
	if !ok {
		return out
	}
	// A provider that declares a features block and leaves vision out says
	// it has none; one that declares nothing (cloud, an endpoint an
	// operator connected) has said nothing, which never refuses a request.
	states := map[string]string{"vision": "unknown"}
	if svc.Features != nil && !svc.IsCloudProvider() {
		states["vision"] = "unsupported"
	}
	for name, f := range svc.Features {
		states[name] = m.modelFeatureState(svc, model, name, f, details)
	}

	// Apply the same tier opt-outs as launch.
	params, _, _ := m.resolveRawLaunchParams(svc, EnsureRequest{Provider: provider, Model: model}, string(EndpointChat))
	for name, f := range svc.Features {
		if value, set := params[f.Flag]; len(f.Files) > 0 && states[name] == "present" && set && (value == "" || config.IsAuto(value) || value == "false") {
			states[name] = "available"
		}
	}
	out["features"] = states
	effective, _, _, err := m.featureRuntime(svc, process.ResolveModel(svc, model), "", string(EndpointChat))
	if err == nil && effective.Capabilities != nil {
		out["wire_endpoints"] = append([]string{}, wireEndpoints(effective)...)
	}
	// A running process retains the endpoints it actually launched with.
	for _, inst := range m.instances.List() {
		cfg := inst.SnapshotConfig()
		if cfg.Provider == provider && cfg.Model == model && EndpointOrDefault(Endpoint(inst.Endpoint)) == EndpointChat && inst.IsRunning() {
			if tokens := inst.Loaded().ContextLength; tokens > 0 {
				out["context_length"] = tokens
			}
			if eps := inst.Resolved().WireEndpoints; eps != nil {
				out["wire_endpoints"] = eps
				snapshot := inst.Resolved()
				for name, f := range svc.Features {
					if states[name] != "present" {
						continue
					}
					if len(f.Files) > 0 && (snapshot.Parameters[f.Flag] == "" || snapshot.Parameters[f.Flag] == "false") {
						states[name] = "available"
					}
					if f.Runtime != "" && config.FeatureEndpoint(name) == string(EndpointChat) && !slices.Equal(eps, config.EffectiveWireEndpoints(f.WireEndpoints)) {
						states[name] = "available"
					}
				}
			}
		}
	}
	return out
}

func (m *ProviderAppManager) modelFeatureState(svc config.ServiceConfig, model, name string, f config.Feature, details map[string]any) string {
	dir := process.ResolveModel(svc, model).Directory()
	state := "unknown"
	switch {
	case len(f.Files) > 0:
		resolved, err := LocalizeModelFeatures(svc, model, nil)
		if err == nil && resolved.Sources[f.Flag] == "feature:"+name {
			state = "present"
		} else if vision, known := details["vision"].(bool); known && name == "vision" {
			state = "unsupported"
			if vision {
				state = "available"
			}
		}
	case f.Runtime != "":
		if dir != "" {
			qualifies, err := featurePredicate(dir, f.When)
			if err == nil {
				state = "unsupported"
				if qualifies {
					state = "available"
					installer, err := m.installs.Installer(f.Runtime)
					if err == nil && installer.IsInstalled() {
						state = "present"
					}
				}
			}
		}
	default:
		if vision, ok := details["vision"].(bool); ok && name == "vision" {
			state = "unsupported"
			if vision {
				state = "present"
			}
		}
	}
	return state
}
