package prov_apps

import (
	"context"
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
				loaded := run.Loaded()
				loaded.ContextLength = tokens
				run.SetLoaded(loaded)
			}
			return
		}
	}
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
