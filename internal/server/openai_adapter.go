// OpenAIAdapter implements ProtocolAdapter for the /v1/* OpenAI-compat
// surface. It owns the mapping from canonical cache.CachedModel /
// ShowModelResponse shapes to OpenAI's wire format, including the zzRouter
// vendor extension and capability derivation.
package server

import (
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/utils"
)

// OpenAIAdapter is a zero-state translator. The AppsConfig lookup it needs
// is injected via closure so tests can drive it without wiring the whole
// app manager.
type OpenAIAdapter struct {
	appsConfig func() *pkgConfig.AppsConfig
}

// NewOpenAIAdapter constructs the adapter with its config dependency.
func NewOpenAIAdapter(appsConfig func() *pkgConfig.AppsConfig) *OpenAIAdapter {
	return &OpenAIAdapter{appsConfig: appsConfig}
}

// ListEntry translates a cache.CachedModel into an OpenAIModelObject, including
// derived capabilities and the zzRouter vendor extension.
//
// The ID is node-qualified as `name@node` for cluster-resident models so
// the same model on multiple nodes surfaces as distinct, directly-
// routable entries (the @suffix form is already accepted by the routing
// layer on /api/* and /v1/*). Cloud entries keep the bare name: the
// brokering node is an implementation detail (a cloud relay isn't
// "served by" the coord, it's brokered through it), and qualifying it
// would make the id unstable across coord renames.
func (a *OpenAIAdapter) ListEntry(m *cache.CachedModel) any {
	created := utils.Now().Unix()
	if !m.Modified.IsZero() {
		created = m.Modified.Unix()
	}
	id := m.Name
	if m.Node != "" && !m.IsCloud {
		id = m.Name + "@" + m.Node
	}
	obj := newOpenAIModel(id, created, "zzrouter")

	cfg := a.appsConfig()
	caps := deriveCapabilities(m, cfg)
	obj.Capabilities = &caps
	obj.ZZRouter = &OpenAIZZRouterVendor{
		Node:     m.Node,
		Provider: m.Provider,
		Protocol: lookupProtocol(m, cfg),
	}
	if m.VariantOf != "" {
		obj.VariantOf = qualifyID(m.VariantOf, m)
	}
	return obj
}

// ListEnvelope wraps the translated entries in OpenAI's list envelope.
// The concrete []OpenAIModelObject rebuild is needed because Gin's encoder
// preserves element types; we want "data" to be a typed array, not []any.
func (a *OpenAIAdapter) ListEnvelope(entries []any) any {
	data := make([]OpenAIModelObject, 0, len(entries))
	for _, e := range entries {
		if obj, ok := e.(OpenAIModelObject); ok {
			data = append(data, obj)
		}
	}
	return OpenAIModelListResponse{Object: "list", Data: data}
}

// ShowResponse translates ShowModelResponse into OpenAI's show shape.
// OpenAI does not expose a detailed model-info endpoint with a standard
// shape; /v1/models/:id returns the same OpenAIModelObject as list. This
// method is provided for interface completeness; /v1 handlers that need
// a single model by ID build it from cache.CachedModel via ListEntry directly.
func (a *OpenAIAdapter) ShowResponse(r *ShowModelResponse) any {
	if r == nil {
		return nil
	}
	// For consistency with ListEntry, return the details map unchanged —
	// OpenAI has no standardized show schema, so passing provider details
	// through as-is is the honest choice.
	return r.Details
}
