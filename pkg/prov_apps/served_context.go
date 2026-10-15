package prov_apps

import (
	"context"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

func (m *ProviderAppManager) readServedContext(run *instance.Instance) {
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
	endpoint, upstream, err := m.Upstream(run)
	if err != nil {
		return
	}
	models, err := p.ListModels(ctx, protocol.NewTarget(endpoint, upstream).Ready())
	if err != nil || ctx.Err() != nil {
		return
	}
	for _, model := range models {
		if model.FullID == run.WireModel {
			if tokens := servedContext(model.Extra, field); tokens > 0 {
				run.UpdateLoaded(func(loaded *instance.Loaded) { loaded.ContextLength = tokens })
			}
			return
		}
	}
}

func recordServedLayers(run *instance.Instance, values []string) {
	gpu, gpuErr := strconv.ParseInt(values[0], 10, 64)
	total, totalErr := strconv.ParseInt(values[1], 10, 64)
	if gpuErr != nil || totalErr != nil || total <= 0 || gpu < 0 || gpu > total {
		return
	}
	run.UpdateLoaded(func(loaded *instance.Loaded) { loaded.GPULayers, loaded.Layers = gpu, total })
}

func servedContext(entry map[string]any, field string) int {
	var value any = entry
	for _, part := range strings.Split(field, ".") {
		object, _ := value.(map[string]any)
		value = object[part]
	}
	n, _ := value.(float64)
	tokens := int(n)
	if tokens > 0 && float64(tokens) == n {
		return tokens
	}
	return 0
}
