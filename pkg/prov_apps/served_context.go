package prov_apps

import (
	"context"
	"math"
	"strconv"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

func (m *ProviderAppManager) readServedContext(run *instance.Instance) {
	endpoint, upstream, err := m.Upstream(run)
	if err != nil {
		return
	}
	target := protocol.NewTarget(endpoint, upstream).Ready()
	for name, evidence := range run.Resolved().FeatureHTTP {
		ctx, cancel := context.WithTimeout(run.Context(), constants.HTTPShortTimeout)
		present, known := target.ReadBool(ctx, m.httpClient, evidence.Path, evidence.Field)
		cancel()
		if known {
			state := "unsupported"
			if present {
				state = "present"
			}
			if run.RecordFeature(name, state) {
				m.refreshCatalog()
			}
		}
	}
	ctx, cancel := context.WithTimeout(run.Context(), constants.HTTPShortTimeout)
	defer cancel()
	key, svc, ok := m.resolveConfigKey(run.Provider)
	if !ok {
		return
	}
	declared, err := m.launchSchema(key)
	if err != nil {
		return
	}
	field := declared.ForEndpoint(run.Endpoint)[svc.GetContextParam()].Served
	p, ok := m.Protocol(key)
	if field == "" || !ok {
		return
	}
	models, err := p.ListModels(ctx, target)
	if err != nil || ctx.Err() != nil {
		return
	}
	for _, model := range models {
		if model.FullID == run.WireModel {
			if tokens := servedContext(model.Extra, field); tokens > 0 {
				if run.UpdateLoaded(func(loaded *instance.Loaded) { loaded.ContextLength = tokens }) {
					m.refreshCatalog() // read after publication notified the catalog
				}
			}
			return
		}
	}
}

const (
	placementLayers = iota
	placementCPUOverrides
	placementWeights
)

func recordPlacement(run *instance.Instance, index int, values []string) {
	run.UpdateLoaded(func(loaded *instance.Loaded) {
		switch index {
		case placementLayers:
			gpu, gpuErr := strconv.ParseInt(values[0], 10, 64)
			total, totalErr := strconv.ParseInt(values[1], 10, 64)
			if gpuErr == nil && totalErr == nil && total > 0 && gpu >= 0 && gpu <= total {
				loaded.GPULayers, loaded.Layers = gpu, total
			}
		case placementCPUOverrides:
			loaded.CPUOverrides = true
		case placementWeights:
			mib, err := strconv.ParseFloat(values[1], 64)
			bytes := mib * constants.BytesPerMB
			// Reject non-finite sizes and leave room for all weight buffers.
			if err != nil || !(bytes > 0 && bytes < float64(math.MaxInt64-loaded.CPUWeights-loaded.GPUWeights)) {
				return
			}
			if values[0] != "" {
				loaded.CPUWeights += int64(bytes)
			} else {
				loaded.GPUWeights += int64(bytes)
			}
		}
	})
}

func servedContext(entry map[string]any, field string) int {
	n, _ := protocol.Field(entry, field).(float64)
	tokens := int(n)
	if tokens > 0 && float64(tokens) == n {
		return tokens
	}
	return 0
}
