package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/dispatch/steering"
	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
)

// routesFieldDTO mirrors schemaParamDTO/accessFieldDTO for the routes
// agent-control surface. Flat type + min/max + enum keeps the wire shape
// uniform across the three schema endpoints.
type routesFieldDTO struct {
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Min         any      `json:"min,omitempty"`
	Max         any      `json:"max,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// routesSchemaResponse is the flat {fields, enums} contract for
// GET /zzrouter/v1/model-groups/schema. Fields are the writable subset
// for PATCH; enums advertise the closed sets used elsewhere on the
// wire so an agent can build a local validator and avoid the round
// trip on every request.
type routesSchemaResponse struct {
	Fields map[string]routesFieldDTO `json:"fields"`
	Enums  map[string][]string       `json:"enums"`
}

// strategyEnum returns the strategy closed set; sources: modelgroup
// StrategyPriority/LeastLoad/Fastest. Phase 8 will add `cheapest`.
func strategyEnum() []string {
	return []string{
		string(modelgroup.StrategyPriority),
		string(modelgroup.StrategyLeastLoad),
		string(modelgroup.StrategyFastest),
	}
}

// cooldownReasonEnum is the *cause* tag stamped on a cooldown entry
// when a backend response forces the dispatcher to mark a replica
// unavailable. Lives at pkg/fallback/cooldown.go:Reason*.
func cooldownReasonEnum() []string {
	return []string{
		pfallback.ReasonRateLimit,
		pfallback.ReasonQuota,
		pfallback.ReasonUnavailable,
		pfallback.ReasonTransport,
	}
}

// attemptSkipReasonEnum is the pre-dispatch filter vocabulary — why
// a candidate was rejected before being tried. Disjoint from
// cooldownReasonEnum: this names the *filter dimension* that fired
// (the replica is in cooldown / unhealthy / tag-mismatch / excluded /
// over latency budget), while cooldownReasonEnum names *what put it
// in cooldown*.
func attemptSkipReasonEnum() []string {
	return []string{
		string(pfallback.SkipReasonCooldown),
		string(pfallback.SkipReasonProviderCooldown),
		string(pfallback.SkipReasonUnhealthy),
		string(pfallback.SkipReasonTagMismatch),
		string(pfallback.SkipReasonExcluded),
		string(pfallback.SkipReasonLatencyBudget),
	}
}

// steeringHeaderEnum advertises the supported X-Route-* per-call hints.
// Only headers the engine actually honors land here — Require-
// Capabilities, Max-Cost-Micro, Prefer-Tags, Prefer-Model are reserved
// for later arcs and rejected at parse time with code
// unsupported_steering_header.
func steeringHeaderEnum() []string {
	return []string{
		steering.HeaderExcludeReplicas,
		steering.HeaderLatencyBudgetMs,
		steering.HeaderRequireTags,
	}
}

// eventTypeEnum is the closed set of EventTypes fan-out via
// /model-groups/events SSE. Pulled from pkg/observability/route_events
// so agents can build a server-side filter (?event_type=glob) without
// scraping every SSE chunk for novel values.
func eventTypeEnum() []string {
	all := route_events.AllEventTypes()
	out := make([]string, len(all))
	for i, et := range all {
		out[i] = string(et)
	}
	return out
}

// steeringAppliedEnum is the parameter-name vocabulary echoed back on
// RoutingMetadata.SteeringApplied. Maps 1:1 to POST /preview body
// fields, so agents that pivot on /preview can share the same
// constants.
func steeringAppliedEnum() []string {
	return []string{
		string(steering.ParamExcludeReplicas),
		string(steering.ParamLatencyBudgetMs),
		string(steering.ParamRequireTags),
	}
}

// attemptOutcomeEnum is the per-attempt tag carried on
// RoutingMetadata.FallbackChain entries. Sources at pfallback.OutcomeSuccess
// / Retriable / NonRetriable. Skip outcomes (cooldown_skip / on_demand_skip)
// are filtered out before reaching the wire chain.
func attemptOutcomeEnum() []string {
	return []string{
		pfallback.OutcomeSuccess,
		pfallback.OutcomeRetriable,
		pfallback.OutcomeNonRetriable,
	}
}

// breakerStateEnum mirrors pkg/fallback/breaker.State string forms.
func breakerStateEnum() []string {
	return []string{"closed", "open", "half_open"}
}

// healthStateEnum exposes the live tri-state derived from
// HealthChecker. "unknown" maps to "no probe has run yet"; healthy /
// unhealthy come from the boolean check.
func healthStateEnum() []string {
	return []string{"healthy", "unhealthy", "unknown"}
}

// errorCodeEnum advertises every closed-enum code this surface can
// emit, so an agent can build a local validator that recognises all of
// them.
//
// Derived from httperr.AllParamErrorCodes rather than hand-listed: the
// previous hand-copy had silently fallen two codes behind
// (unknown_field, route_not_claimed), which is the failure that matters
// here — under-advertising makes a client treat a real code as
// unrecognised, and nothing surfaces the gap.
//
// The steering codes are appended verbatim; they fire on the inference
// path rather than the mutator path, so they do not live in pkg/httperr.
func errorCodeEnum() []string {
	all := httperr.AllParamErrorCodes()
	codes := make([]string, 0, len(all)+2)
	for _, c := range all {
		codes = append(codes, string(c))
	}
	codes = append(codes, "invalid_steering_header", "unsupported_steering_header")
	sort.Strings(codes)
	return codes
}

// routesSchema is the response builder. New fields land here, not in
// the handler, so the test surface mirrors the wire surface 1:1.
func routesSchema() routesSchemaResponse {
	return routesSchemaResponse{
		Fields: map[string]routesFieldDTO{
			"description":                    {Type: "string", Description: "Free-form description; null clears."},
			"strategy":                       {Type: "string", Enum: strategyEnum(), Description: "Replica selection strategy. Null reverts to the server default."},
			"health_check.path":              {Type: "string", Description: "HTTP path probed during background health checks."},
			"health_check.interval":          {Type: "duration", Description: "Probe interval, e.g. '30s'."},
			"health_check.timeout":           {Type: "duration", Description: "Per-probe timeout."},
			"replicas[].name":                {Type: "string", Required: true, Description: "URL-canonical replica identity."},
			"replicas[].model":               {Type: "string", Required: true, Description: "Backend model name as known to the provider."},
			"replicas[].provider":            {Type: "string", Required: true, Description: "Provider/app key (ollama, vllm, …)."},
			"replicas[].node":                {Type: "string", Description: "Target node (empty = local; hostname = remote)."},
			"replicas[].priority":            {Type: "int", Description: "Selection priority for the priority strategy; lower = preferred."},
			"replicas[].timeout":             {Type: "duration", Description: "Per-replica request timeout."},
			"replicas[].on_demand":           {Type: "bool", Description: "Start the local model on first dispatch if not already running."},
			"replicas[].gpu_memory_required": {Type: "string", Description: "GPU memory needed for on-demand starts (informational)."},
			"replicas[].max_retries":         {Type: "int", Min: 0, Description: "Retries on this replica before falling back."},
			"replicas[].tags":                {Type: "string[]", Description: "Routing labels; agents filter via X-Route-Require-Tags."},
			"params":                         {Type: "object", Description: "Extensible per-route parameters; merge-patch per key."},
			"ttl_seconds":                    {Type: "int", Min: 60, Max: 86400, Description: "Ephemeral-route TTL; ExpiresAt = now + ttl. Null on PATCH clears (route becomes perpetual)."},
			"owner":                          {Type: "string", Max: 128, Description: "Free-text audit tag for ephemeral routes; informational only."},
			"expires_at":                     {Type: "string", Description: "Read-only RFC3339 expiry; non-empty for ephemeral routes."},
			// Preview body — POST /model-groups/preview and
			// POST /:name/preview share this input vocabulary. None
			// of these are mutators; they describe a dry-run request.
			"preview.route":             {Type: "string", Description: "Route name (single-route preview); URL :name overrides this."},
			"preview.require_tags":      {Type: "string[]", Description: "Hard tag filter; every replica must carry every tag."},
			"preview.exclude_replicas":  {Type: "string[]", Description: "Per-request replica blocklist."},
			"preview.latency_budget_ms": {Type: "int", Min: 0, Description: "Skip replicas whose observed p50 latency exceeds this budget."},
			"preview.input_tokens":      {Type: "int", Min: 0, Description: "Token count fed to CalculateCostMicro for projected cost."},
			"preview.output_tokens":     {Type: "int", Min: 0, Description: "Generated token count for projected cost."},
		},
		Enums: map[string][]string{
			"strategy":            strategyEnum(),
			"cooldown_reason":     cooldownReasonEnum(),
			"attempt_skip_reason": attemptSkipReasonEnum(),
			"attempt_outcome":     attemptOutcomeEnum(),
			"breaker_state":       breakerStateEnum(),
			"health_state":        healthStateEnum(),
			"error_code":          errorCodeEnum(),
			"steering_header":     steeringHeaderEnum(),
			"steering_applied":    steeringAppliedEnum(),
			"event_type":          eventTypeEnum(),
			// projected_cost_source is the closed-enum tag on every
			// preview assignment's cost. Mirrors quota.CostSource* —
			// empty string means "no authoritative price; agents
			// should treat as unknown".
			"projected_cost_source": {quota.CostSourceProvider, quota.CostSourceZZRouter, ""},
			// Capability is reserved for a later arc; agents see the
			// field but know the vocabulary is not yet populated.
			"capability": {},
		},
	}
}

// GetRoutesSchema handles GET /zzrouter/v1/model-groups/schema.
func (ctrl *ModelGroupsController) GetRoutesSchema(c *gin.Context) {
	resp := routesSchema()
	writeJSONWithETag(c, resp)
}

// writeJSONWithETag marshals body deterministically, computes a
// content-hash ETag, honors If-None-Match with 304, and writes the
// body otherwise. The hash source MUST be a configuration projection:
// don't pass live derived fields (cooldown_seconds, breaker_state, …)
// in payload or the ETag will flicker on every poll.
func writeJSONWithETag(c *gin.Context, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		InternalNodeError(c, "marshal: "+err.Error())
		return
	}
	sum := sha256.Sum256(raw)
	etag := `"sha256:` + hex.EncodeToString(sum[:]) + `"`
	if c.GetHeader("If-None-Match") == etag {
		c.Header("ETag", etag)
		c.Status(304)
		return
	}
	c.Header("ETag", etag)
	c.Header("Content-Type", "application/json")
	_, _ = c.Writer.Write(raw)
}
