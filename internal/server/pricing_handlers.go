package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/model/pricing"
)

// PricingController handles pricing data API endpoints
type PricingController struct {
	store *pricing.Store
}

// NewPricingController creates a new pricing controller
func NewPricingController(store *pricing.Store) *PricingController {
	return &PricingController{store: store}
}

// RegisterRoutes registers pricing routes unconditionally. When the pricing
// store is disabled (no models.pricing.enabled in node.yaml), handlers return
// 503 Problem Details so clients can distinguish "feature disabled" from
// "endpoint does not exist".
func (pc *PricingController) RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/pricing", pc.HandleList)
	router.GET("/pricing/status", pc.HandleStatus)
	// Overrides are keyed by (provider, model) in the body or the query
	// rather than the path: model ids carry slashes ("anthropic/claude-…",
	// "@cf/meta/…") that a gin path param cannot hold.
	router.GET("/pricing/overrides", pc.HandleListOverrides)
	router.POST("/pricing/overrides", pc.HandleUpsertOverride)
	router.DELETE("/pricing/overrides", pc.HandleDeleteOverride)
	router.GET("/pricing/:model", pc.HandleLookup)
}

// disabled returns true and writes a 503 if pricing is not enabled.
func (pc *PricingController) disabled(c *gin.Context) bool {
	if pc.store != nil {
		return false
	}
	ServiceUnavailable(c, "pricing data is not enabled; set models.pricing.enabled: true in node.yaml")
	return true
}

// HandleStatus returns pricing cache metadata (last fetch, model count, etc.)
// GET /zzrouter/v1/pricing/status
func (pc *PricingController) HandleStatus(c *gin.Context) {
	if pc.disabled(c) {
		return
	}
	meta := pc.store.Metadata()
	misses := pc.store.Misses()
	respondSuccess(c, "Pricing status retrieved", gin.H{
		"enabled":      true,
		"model_count":  pc.store.ModelCount(),
		"fetched_at":   meta.FetchedAt,
		"content_hash": meta.ContentHash,
		// Models that served real traffic with no price entry. Each one
		// settled at $0 and drew down no budget, so a non-empty map is
		// an operator action item, not a statistic.
		"unpriced_count": len(misses),
		"unpriced":       misses,
	})
}

// HandleLookup returns pricing for a specific model.
// GET /zzrouter/v1/pricing/:model — works for slash-free names only;
// use GET /pricing?model=<exact> for slash-namespaced names.
// GET /zzrouter/v1/pricing/:model?provider=openai
func (pc *PricingController) HandleLookup(c *gin.Context) {
	if pc.disabled(c) {
		return
	}
	pc.lookup(c, c.Param("model"), c.Query("provider"))
}

// lookup is the shared renderer for both /pricing/:model and the
// /pricing?model=<exact> query-param fallback.
func (pc *PricingController) lookup(c *gin.Context, model, provider string) {
	var p pricing.ModelPricing
	var ok bool
	if provider != "" {
		p, ok = pc.store.LookupByProvider(provider, model)
	} else {
		p, ok = pc.store.Lookup(model)
	}
	if !ok {
		NotFound(c, "no pricing data for model: "+model)
		return
	}
	resp := gin.H{
		"model":                     model,
		"provider":                  p.Provider,
		"mode":                      p.Mode,
		"input_cost_per_token":      p.InputCostPerToken,
		"output_cost_per_token":     p.OutputCostPerToken,
		"input_cost_per_1m":         p.InputCostPer1M(),
		"output_cost_per_1m":        p.OutputCostPer1M(),
		"max_input_tokens":          p.MaxInputTokens,
		"max_output_tokens":         p.MaxOutputTokens,
		"context_window":            p.ContextWindow(),
		"supports_vision":           p.SupportsVision,
		"supports_function_calling": p.SupportsFunctionCalling,
	}
	// Surface cache + reasoning rates only when present so agents can
	// distinguish "cache discount available, but I haven't fetched the
	// rate" from "this model has no cache pricing." omitempty on the
	// numeric fields keeps the wire small.
	if p.CacheReadCostPerToken > 0 {
		resp["cache_read_cost_per_token"] = p.CacheReadCostPerToken
		resp["cache_read_cost_per_1m"] = p.CacheReadCostPerToken * 1_000_000
	}
	if p.CacheCreationCostPerToken > 0 {
		resp["cache_write_cost_per_token"] = p.CacheCreationCostPerToken
		resp["cache_write_cost_per_1m"] = p.CacheCreationCostPerToken * 1_000_000
	}
	if p.OutputCostPerReasoningToken > 0 {
		resp["output_cost_per_reasoning_token"] = p.OutputCostPerReasoningToken
		resp["output_cost_per_reasoning_1m"] = p.OutputCostPerReasoningToken * 1_000_000
	}
	respondSuccess(c, "Pricing retrieved", resp)
}

// HandleList searches or lists pricing data.
// GET /zzrouter/v1/pricing?q=claude&provider=anthropic — search.
// GET /zzrouter/v1/pricing?model=<exact> — exact lookup for a single
// model. Required for slash-namespaced models (meta-llama/llama-3.2-3b-instruct,
// anthropic/claude-3.5-sonnet, etc.) that the path-param route
// /pricing/:model cannot match because gin treats / as a separator.
func (pc *PricingController) HandleList(c *gin.Context) {
	if pc.disabled(c) {
		return
	}
	if exact := c.Query("model"); exact != "" {
		pc.lookup(c, exact, c.Query("provider"))
		return
	}
	query := c.Query("q")
	provider := c.Query("provider")
	limit := 50

	if query == "" && provider == "" {
		respondSuccess(c, "Pricing catalog summary", gin.H{
			"model_count": pc.store.ModelCount(),
			"hint":        "Use ?q=<search>, ?provider=<name>, or ?model=<exact> (slashed names work) to filter",
		})
		return
	}

	results := pc.store.Search(query, provider)

	items := make([]gin.H, 0, limit)
	for name, p := range results {
		if len(items) >= limit {
			break
		}
		items = append(items, gin.H{
			"model":              name,
			"provider":           p.Provider,
			"mode":               p.Mode,
			"input_cost_per_1m":  p.InputCostPer1M(),
			"output_cost_per_1m": p.OutputCostPer1M(),
			"max_input_tokens":   p.MaxInputTokens,
			"max_output_tokens":  p.MaxOutputTokens,
		})
	}

	respondListWithMetadata(c, items, len(results), len(items) < len(results), map[string]any{
		"query":    query,
		"provider": provider,
	})
}

// perMillion is the unit the override API speaks. Rates are stored per
// token, but nobody hand-writes 0.0000025 — requests and responses use
// dollars per million tokens, converted at the edge.
const perMillion = 1_000_000

// OverrideRequest is the POST body for an operator price override.
// Rates are dollars per million tokens; omitted rates mean zero, which
// is a meaningful value here (see the zero-rate note on the response).
type OverrideRequest struct {
	Provider           string  `json:"provider,omitempty"`
	Model              string  `json:"model"`
	InputCostPer1M     float64 `json:"input_cost_per_1m"`
	OutputCostPer1M    float64 `json:"output_cost_per_1m"`
	CacheReadCostPer1M float64 `json:"cache_read_cost_per_1m,omitempty"`
	Note               string  `json:"note,omitempty"`
}

// overrideResponse renders an override in both units so callers can read
// the human number and the billed number without converting.
func overrideResponse(o pricing.Override) gin.H {
	h := gin.H{
		"model":                 o.Model,
		"input_cost_per_1m":     o.InputCostPerToken * perMillion,
		"output_cost_per_1m":    o.OutputCostPerToken * perMillion,
		"input_cost_per_token":  o.InputCostPerToken,
		"output_cost_per_token": o.OutputCostPerToken,
		"billed_per_token":      o.InputCostPerToken > 0 || o.OutputCostPerToken > 0,
		"applies_to_all_models": o.Model == pricing.WildcardModel,
	}
	if o.Provider != "" {
		h["provider"] = o.Provider
	}
	if o.CacheReadCostPerToken > 0 {
		h["cache_read_cost_per_1m"] = o.CacheReadCostPerToken * perMillion
		h["cache_read_cost_per_token"] = o.CacheReadCostPerToken
	}
	if o.Note != "" {
		h["note"] = o.Note
	}
	if !o.UpdatedAt.IsZero() {
		h["updated_at"] = o.UpdatedAt
	}
	if o.UpdatedBy != "" {
		h["updated_by"] = o.UpdatedBy
	}
	return h
}

// HandleListOverrides returns every operator price override.
// GET /zzrouter/v1/pricing/overrides
func (pc *PricingController) HandleListOverrides(c *gin.Context) {
	if pc.disabled(c) {
		return
	}
	list := pc.store.ListOverrides()
	out := make([]gin.H, len(list))
	for i, o := range list {
		out[i] = overrideResponse(o)
	}
	respondSuccess(c, "Pricing overrides retrieved", gin.H{
		"overrides": out,
		"count":     len(out),
	})
}

// HandleUpsertOverride adds or replaces an override. Upsert rather than
// separate create/update: the (provider, model) pair is the identity, so
// a second POST for the same pair is a correction, not a conflict.
// POST /zzrouter/v1/pricing/overrides
//
// An all-zero override is legal and load-bearing: it declares a provider
// that does not bill per token (a subscription plan, or local hardware),
// which stops its traffic being reported as un-priced.
func (pc *PricingController) HandleUpsertOverride(c *gin.Context) {
	if pc.disabled(c) {
		return
	}
	var req OverrideRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	o := pricing.Override{
		Provider:              req.Provider,
		Model:                 req.Model,
		InputCostPerToken:     req.InputCostPer1M / perMillion,
		OutputCostPerToken:    req.OutputCostPer1M / perMillion,
		CacheReadCostPerToken: req.CacheReadCostPer1M / perMillion,
		Note:                  req.Note,
		UpdatedBy:             auditActor(c),
	}
	if err := pc.store.SetOverride(o); err != nil {
		BadRequest(c, err.Error())
		return
	}
	respondSuccess(c, "Pricing override saved", overrideResponse(o))
}

// HandleDeleteOverride removes an override, restoring upstream pricing.
// DELETE /zzrouter/v1/pricing/overrides?model=<id>&provider=<name>
func (pc *PricingController) HandleDeleteOverride(c *gin.Context) {
	if pc.disabled(c) {
		return
	}
	model := c.Query("model")
	if model == "" {
		BadRequest(c, "model query parameter is required")
		return
	}
	removed, err := pc.store.DeleteOverride(c.Query("provider"), model)
	if err != nil {
		BadRequest(c, err.Error())
		return
	}
	if !removed {
		NotFound(c, "no pricing override for model: "+model)
		return
	}
	respondSuccess(c, "Pricing override deleted", gin.H{
		"model":    model,
		"provider": c.Query("provider"),
	})
}
