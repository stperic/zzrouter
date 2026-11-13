package server

import (
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/fallback"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/model/resolver"
)

// previewAssignmentsCap bounds the response size of the top-level
// cross-route preview endpoint. With N groups in the store, an
// unbounded walk would let an admin DoS the preview surface; capping
// at 25 keeps the response shape predictable and the worst-case work
// finite. Caller picks `route` for narrower lookups.
const previewAssignmentsCap = 25

// previewRequest is the body shape for POST /model-groups/preview
// (top-level cross-route search) and POST /model-groups/:name/preview
// (single-route). All fields are optional; an empty body still
// produces an alphabetical sweep over the route table up to the cap.
//
// Preview is body-only on purpose: X-Route-* steering headers are for
// live dispatch. Mixing the two input channels in a dry-run primitive
// muddies the contract.
type previewRequest struct {
	TeamID string `json:"team_id,omitempty"`
	// Route narrows the sweep to a single named route. The URL :name
	// override fills this for the per-route handler; the top-level
	// handler treats it as a body-supplied filter.
	Route string `json:"route,omitempty"`

	// RequireTags is the hard tag filter — every replica must carry
	// every tag in this list to survive.
	RequireTags []string `json:"require_tags,omitempty"`

	// ExcludeReplicas is the retry primitive: any replica name in
	// this list is filtered out regardless of other state.
	ExcludeReplicas []string `json:"exclude_replicas,omitempty"`

	// LatencyBudgetMs caps a replica by observed p50 latency; 0 means
	// no budget filter.
	LatencyBudgetMs int `json:"latency_budget_ms,omitempty"`

	// InputTokens / OutputTokens drive the projected cost computation
	// via the pricing store. Both default to 0 — the response carries
	// projected_cost_micro=0 + projected_cost_source="" when either
	// the token count is zero or the pricing store has no entry.
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// previewSkip mirrors fallback.Skip for the wire response — the
// replica name and the closed-enum SkipReason that filtered it out.
type previewSkip struct {
	Replica string `json:"replica"`
	Reason  string `json:"reason"`
}

// previewAssignment is one chosen route × replica pair plus its
// projected cost. Skipped replicas are echoed so agents can see what
// they lost and why; the chosen replica is at the top of the list
// per group strategy.
type previewAssignment struct {
	Eligible            []string      `json:"eligible,omitempty"`
	DeniedReason        string        `json:"denied_reason,omitempty"`
	Route               string        `json:"route"`
	RouteVersion        string        `json:"route_version"`
	Replica             string        `json:"replica,omitempty"`
	ProjectedCostMicro  int64         `json:"projected_cost_micro"`
	ProjectedCostSource string        `json:"projected_cost_source"`
	Strategy            string        `json:"strategy"`
	Skipped             []previewSkip `json:"skipped,omitempty"`
	// NoSurvivors signals "every replica was filtered out"; agents see
	// the Skipped list and the absent Replica field together.
	NoSurvivors bool `json:"no_survivors,omitempty"`
}

// previewResponse is the wire envelope. total_projected_cost_micro is
// omitted when any assignment has an empty source (pricing data
// missing); summing partial-priced assignments would mislead.
type previewResponse struct {
	TeamID                  string              `json:"team_id,omitempty"`
	Unchecked               []string            `json:"unchecked"`
	Assignments             []previewAssignment `json:"assignments"`
	TotalProjectedCostMicro *int64              `json:"total_projected_cost_micro,omitempty"`
	Capped                  bool                `json:"capped,omitempty"`
}

// PreviewRoute serves POST /zzrouter/v1/model-groups/:name/preview —
// a single-route deterministic dry-run. Same handler shape as the
// top-level preview with `route` pre-bound from the URL. The
// response's assignments slice has length 0 (route missing) or 1.
func (ctrl *ModelGroupsController) PreviewRoute(c *gin.Context) {
	name := c.Param("name")
	var req previewRequest
	if !readPreviewBody(c, &req) {
		return
	}
	req.Route = name
	ctrl.runPreview(c, req)
}

// PreviewRoutes serves POST /zzrouter/v1/model-groups/preview —
// cross-route preview. Walks every route alphabetically up to
// previewAssignmentsCap and applies the same filter+strategy as a
// live dispatch would, returning one assignment per route.
//
// Determinism: no cooldown/breaker mutation, no LatencyTracker
// observation, no audit log emission. Two identical requests against
// identical state must produce identical responses.
func (ctrl *ModelGroupsController) PreviewRoutes(c *gin.Context) {
	var req previewRequest
	if !readPreviewBody(c, &req) {
		return
	}
	ctrl.runPreview(c, req)
}

// readPreviewBody decodes the body, or writes a 400 and returns false.
// Empty body is allowed — produces a sweep over all routes.
func readPreviewBody(c *gin.Context, req *previewRequest) bool {
	if c.Request.ContentLength == 0 {
		return true
	}
	if !BindJSONStrict(c, req) {
		return false
	}
	if req.InputTokens < 0 || req.OutputTokens < 0 || req.LatencyBudgetMs < 0 {
		BadRequest(c, "Token counts and latency budget must be nonnegative")
		return false
	}
	return true
}

// runPreview is the shared dispatch — emits the assignment slice and
// totals. Holds no locks beyond a single GroupStore.List() snapshot,
// so concurrent mutators don't block on preview reads.
func (ctrl *ModelGroupsController) runPreview(c *gin.Context, req previewRequest) {
	var ac *AccessContext
	if req.TeamID != "" {
		if ctrl.access != nil {
			ac = ctrl.access.core.TeamContext(req.TeamID)
		}
		if ac == nil || ac.Team == nil {
			NotFound(c, "team not found")
			return
		}
	}

	all := map[string]modelgroup.ModelGroup{}
	if ctrl.groupStore != nil {
		all = ctrl.groupStore.List()
	}
	names := previewRouteNames(all, req.Route)
	sort.Strings(names)

	capped := false
	if len(names) > previewAssignmentsCap {
		names = names[:previewAssignmentsCap]
		capped = true
	}

	assignments := make([]previewAssignment, 0, len(names))
	for _, n := range names {
		g := all[n]
		assignments = append(assignments, ctrl.previewOne(n, g, req, ac))
	}

	resp := previewResponse{Assignments: assignments, Capped: capped, TeamID: req.TeamID, Unchecked: []string{"key_limits", "rate_limits", "concurrency", "endpoint_and_input", "model_readiness", "memory_capacity"}}
	if total, ok := previewTotal(assignments); ok {
		resp.TotalProjectedCostMicro = &total
	}
	respondSuccess(c, "preview computed", resp)
}

// previewRouteNames returns the candidate route name set. If `only`
// is set, returns just that name (possibly empty when the route is
// absent — the caller surfaces that as len(assignments)=0).
func previewRouteNames(all map[string]modelgroup.ModelGroup, only string) []string {
	if only != "" {
		if _, ok := all[only]; !ok {
			return nil
		}
		return []string{only}
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	return names
}

// previewOne runs the filter + strategy + cost computation for a
// single route. The cooldown/health/latency dependencies are read-
// only — the same snapshot live dispatch would see, but the preview
// path never writes back.
func (ctrl *ModelGroupsController) previewOne(name string, g modelgroup.ModelGroup, req previewRequest, ac *AccessContext) previewAssignment {
	a := previewAssignment{Route: name, RouteVersion: modelGroupETag(name, g), Strategy: strategyName(g.Strategy)}
	if !g.Available() {
		a.NoSurvivors = true
		a.DeniedReason = "route_unavailable"
		return a
	}
	if ac != nil {
		if decision := ctrl.access.core.Authorize(ac, name); !decision.Allowed {
			a.NoSurvivors = true
			a.DeniedReason = decision.Reason.String()
			return a
		}
	}
	cands := resolver.GroupCandidates(g.Replicas)
	allowed := cands[:0:0]
	for _, cand := range cands {
		reason := ""
		if !control.ProviderAllowed(ac, cand.App) {
			reason = "provider_access_denied"
		} else if ac != nil && !ctrl.access.core.DestinationAllowed(ac, cand.Model, cand.App) {
			reason = "model_access_denied"
		}
		if reason != "" {
			a.Skipped = append(a.Skipped, previewSkip{Replica: cand.Name, Reason: reason})
		} else {
			allowed = append(allowed, cand)
		}
	}
	quote := ctrl.quote
	if quote == nil {
		quote = func(provider, model string) pricing.Quote {
			return pricing.Resolve(ctrl.pricing, provider, []string{model}, false)
		}
	}
	allowed, quotes, unpriced := pricedCandidates(allowed, control.HasSpendLimit(ac), quote)
	for _, cand := range unpriced {
		a.Skipped = append(a.Skipped, previewSkip{Replica: cand.Name, Reason: "model_unpriced"})
	}
	if len(allowed) == 0 {
		a.NoSurvivors = true
		return a
	}
	if ac != nil {
		if decision := ctrl.access.core.PreviewTeamBudget(ac, quotes); !decision.Allowed {
			a.DeniedReason = decision.Reason
			return a
		}
	}
	survivors, skipped := fallback.FilterDeployments(allowed, ctrl.cooldowns, ctrl.providerCooldowns, ctrl.healthChecker,
		req.RequireTags, ctrl.latencyTracker, fallback.SteeringHints{ExcludeReplicas: req.ExcludeReplicas, LatencyBudgetMs: req.LatencyBudgetMs})
	a.Skipped = append(a.Skipped, previewSkipsFromFallback(skipped)...)
	if len(survivors) == 0 {
		a.NoSurvivors = true
		return a
	}
	ordered := previewStrategy(a.Strategy, ctrl.latencyTracker, ctrl.loadTracker).Select(survivors)
	for _, candidate := range ordered {
		a.Eligible = append(a.Eligible, candidate.Name)
	}
	chosen := ordered[0]
	a.Replica = chosen.Name
	a.ProjectedCostMicro, a.ProjectedCostSource = projectedCost(ctrl.pricing, chosen, req.InputTokens, req.OutputTokens, quote(chosen.App, chosen.Model))
	return a
}

func previewSkipsFromFallback(skipped []fallback.Skip) []previewSkip {
	if len(skipped) == 0 {
		return nil
	}
	out := make([]previewSkip, len(skipped))
	for i, s := range skipped {
		out[i] = previewSkip{Replica: s.Candidate.Name, Reason: string(s.Reason)}
	}
	return out
}

// previewStrategy uses the same read-only strategy implementations as dispatch.
func previewStrategy(strategy string, latency fallback.LatencySnapshotter, loads fallback.LoadSnapshotter) fallback.DeploymentStrategy {
	switch strategy {
	case "fastest":
		return fallback.NewFastestStrategy(latency)
	case "least-load":
		return fallback.NewLeastLoadStrategy(loads)
	default:
		return &fallback.PriorityStrategy{}
	}
}

// projectedCost computes a body-driven cost projection for a chosen
// replica. providerCost=nil forces CalculateCostMicro down the pricing
// store path; an empty source means the store has no entry for this
// (provider, model) pair — agents see projected_cost_micro=0 +
// projected_cost_source="" and treat it as "unknown".
func projectedCost(store *pricing.Store, c fallback.Candidate, in, out int64, admitted ...pricing.Quote) (int64, string) {
	if in == 0 && out == 0 {
		return 0, ""
	}
	return quota.CalculateCostMicro(nil, store, c.App, []string{c.Model}, in, out, 0, 0, admitted...)
}

// strategyName mirrors live dispatch's fallback in
// pkg/dispatch/chain/fallback.go: empty or unknown strategies resolve
// to priority so the response strategy field is always one of the
// closed-enum values agents validate against.
func strategyName(s modelgroup.StrategyType) string {
	switch s {
	case modelgroup.StrategyPriority, modelgroup.StrategyLeastLoad, modelgroup.StrategyFastest:
		return string(s)
	default:
		return string(modelgroup.StrategyPriority)
	}
}

// previewTotal returns the sum of projected costs and a flag for
// whether the sum is meaningful (every assignment has a non-empty
// source). When any source is empty the total is omitted.
func previewTotal(assignments []previewAssignment) (int64, bool) {
	var total int64
	for _, a := range assignments {
		if a.NoSurvivors || a.DeniedReason != "" {
			continue
		}
		if a.ProjectedCostSource == "" {
			return 0, false
		}
		total += a.ProjectedCostMicro
	}
	return total, true
}
