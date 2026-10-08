// GET /v1/model/info — model introspection with routing metadata.
//
// zzrouter exposes two complementary model-listing endpoints:
//
//   - /v1/models returns OpenAI's canonical {id, object, created, owned_by}
//     plus zzrouter's capabilities vendor extension. It is the endpoint
//     OpenAI SDKs call on startup to discover available models.
//
//   - /v1/model/info returns an envelope with per-model routing metadata
//     (which provider, which node, which protocol) and the full capability
//     object. It is intended for admin and introspection tooling that needs
//     to see how each public model name is wired to a backend.
//
// The underlying data source is identical — the same ListModels service
// call — so the two endpoints cannot drift on which models are advertised.
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
)

// OpenAIModelInfoEntry is one row in the /v1/model/info response.
//
// Field layout:
//   - model_name: the public name clients call, matches the id used in
//     /v1/models and /v1/chat/completions.
//   - routing:    zzrouter-specific routing metadata (node, provider,
//     protocol).
//   - model_info: the full capability dict plus a synthetic id derived from
//     the model name.
type OpenAIModelInfoEntry struct {
	ModelName string                   `json:"model_name"`
	Routing   OpenAIZZRouterVendor     `json:"routing"`
	ModelInfo OpenAIModelInfoInnerBody `json:"model_info"`
}

// OpenAIModelInfoInnerBody is the nested `model_info` object.
//
// `DBModel` is always false for zzrouter: the model catalog is
// derived from provider config and the cluster's runtime registry, not
// a database. The field is emitted because ecosystem tooling that
// follows LiteLLM's model-info convention keys on it to distinguish
// config-sourced models from admin-mutable ones.
type OpenAIModelInfoInnerBody struct {
	ID           string                  `json:"id"`
	Capabilities OpenAIModelCapabilities `json:"capabilities"`
	Created      int64                   `json:"created"`
	DBModel      bool                    `json:"db_model"`
}

// OpenAIModelInfoResponse wraps the list in a `data` envelope.
type OpenAIModelInfoResponse struct {
	Data []OpenAIModelInfoEntry `json:"data"`
}

// handleModelInfo handles GET /v1/model/info. It reuses the same ListModels
// service call as /v1/models so the two endpoints cannot drift.
func (h *OpenAIModelHandlers) handleModelInfo(c *gin.Context) {
	modelsResp, err := h.modelService.ListModels(c.Request.Context(), &ListModelsRequest{})
	if err != nil {
		h.respondInternal(c, "model info: "+err.Error())
		return
	}

	cfg := h.appsConfig()
	data := make([]OpenAIModelInfoEntry, 0, len(modelsResp))
	for _, m := range modelsResp {
		created := utils.Now().Unix()
		if !m.Modified.IsZero() {
			created = m.Modified.Unix()
		}
		data = append(data, OpenAIModelInfoEntry{
			ModelName: m.Name,
			Routing: OpenAIZZRouterVendor{
				Node:     m.Node,
				Provider: m.Provider,
				Protocol: lookupProtocol(m, cfg),
			},
			ModelInfo: OpenAIModelInfoInnerBody{
				ID:           m.Name,
				Capabilities: deriveCapabilities(m, cfg),
				Created:      created,
			},
		})
	}

	c.JSON(http.StatusOK, OpenAIModelInfoResponse{Data: data})
}
