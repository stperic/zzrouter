package server

import (
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/fallback"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
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
	Route               string        `json:"route"`
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
	return BindJSONStrict(c, req)
}

// runPreview is the shared dispatch — emits the assignment slice and
// totals. Holds no locks beyond a single GroupStore.List() snapshot,
// so concurrent mutators don't block on preview reads.
func (ctrl *ModelGroupsController) runPreview(c *gin.Context, req previewRequest) {
	if ctrl.groupStore == nil {
		respondSuccess(c, "no model groups configured", previewResponse{Assignments: []previewAssignment{}})
		return
	}

	all := ctrl.groupStore.List()
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
		assignments = append(assignments, ctrl.previewOne(n, g, req))
	}

	resp := previewResponse{Assignments: assignments, Capped: capped}
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
func (ctrl *ModelGroupsController) previewOne(name string, g modelgroup.ModelGroup, req previewRequest) previewAssignment {
	cands := previewCandidatesFromReplicas(g.Replicas)
	hints := fallback.SteeringHints{
		ExcludeReplicas: req.ExcludeReplicas,
		LatencyBudgetMs: req.LatencyBudgetMs,
	}
	survivors, skipped := fallback.FilterDeployments(
		cands,
		ctrl.cooldowns,
		nil, // providerCooldowns — Phase 6 preview omits the provider dimension
		ctrl.healthChecker,
		req.RequireTags,
		ctrl.latencyTracker,
		hints,
	)

	strategy := strategyName(g.Strategy)
	a := previewAssignment{
		Route:    name,
		Strategy: strategy,
		Skipped:  previewSkipsFromFallback(skipped),
	}
	if len(survivors) == 0 {
		a.NoSurvivors = true
		return a
	}

	chosen := pickPreviewWinner(survivors, strategy, ctrl.latencyTracker, ctrl.loadTracker)
	a.Replica = chosen.Name
	cost, source := projectedCost(ctrl.pricingStore(), chosen, req.InputTokens, req.OutputTokens)
	a.ProjectedCostMicro = cost
	a.ProjectedCostSource = source
	return a
}

// pricingStore returns the controller's pricing dependency. Indirected
// so a nil controller field falls through to CalculateCostMicro's
// "no source" path without panicking.
func (ctrl *ModelGroupsController) pricingStore() *pricing.Store {
	return ctrl.pricing
}

func previewCandidatesFromReplicas(reps []modelgroup.Replica) []fallback.Candidate {
	out := make([]fallback.Candidate, len(reps))
	for i, r := range reps {
		out[i] = fallback.Candidate{
			Name:       r.Name,
			Model:      r.Model,
			App:        r.App,
			Node:       r.Node,
			Priority:   r.Priority,
			Timeout:    r.Timeout.Duration,
			OnDemand:   r.OnDemand,
			MaxRetries: r.MaxRetries,
			Tags:       r.Tags,
		}
	}
	return out
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

// pickPreviewWinner emulates the strategy for the preview without
// touching tracker state. priority → lowest .Priority field; fastest
// → lowest LatencyTracker snapshot avg; least-load → lowest in-flight
// load; unknown → first survivor (deterministic via filter order).
func pickPreviewWinner(
	survivors []fallback.Candidate,
	strategy string,
	latency fallback.LatencySnapshotter,
	loads loadSnapshotter,
) fallback.Candidate {
	switch strategy {
	case string(modelgroup.StrategyPriority):
		best := survivors[0]
		for _, c := range survivors[1:] {
			if c.Priority < best.Priority {
				best = c
			}
		}
		return best
	case string(modelgroup.StrategyFastest):
		if latency == nil {
			return survivors[0]
		}
		best := survivors[0]
		bestAvg, bestSamples := latency.Snapshot(best.Name)
		for _, c := range survivors[1:] {
			avg, samples := latency.Snapshot(c.Name)
			// Mirror strategy_fastest's sort exactly: replicas with
			// samples sort ahead of untried ones ("Deployments with no
			// data go to the end"), then by ascending average latency.
			// Survivors already arrive in priority order, so keeping the
			// incumbent on a tie reproduces the live priority tiebreak.
			switch {
			case bestSamples == 0 && samples > 0:
				best, bestAvg, bestSamples = c, avg, samples
			case bestSamples > 0 && samples > 0 && avg < bestAvg:
				best, bestAvg, bestSamples = c, avg, samples
			}
		}
		return best
	case string(modelgroup.StrategyLeastLoad):
		if loads == nil {
			return survivors[0]
		}
		best := survivors[0]
		bestLoad := loads.InFlight(best.Name)
		for _, c := range survivors[1:] {
			if l := loads.InFlight(c.Name); l < bestLoad {
				best, bestLoad = c, l
			}
		}
		return best
	default:
		return survivors[0]
	}
}

// loadSnapshotter is the narrow read interface pickPreviewWinner
// needs from *fallback.LoadTracker for the least-load strategy.
type loadSnapshotter interface {
	InFlight(name string) int64
}

// projectedCost computes a body-driven cost projection for a chosen
// replica. providerCost=0 forces CalculateCostMicro down the pricing
// store path; an empty source means the store has no entry for this
// (provider, model) pair — agents see projected_cost_micro=0 +
// projected_cost_source="" and treat it as "unknown".
func projectedCost(store *pricing.Store, c fallback.Candidate, in, out int64) (int64, string) {
	if in == 0 && out == 0 {
		return 0, ""
	}
	return quota.CalculateCostMicro(0, store, c.App, []string{c.Model}, in, out, 0, 0)
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
		if a.NoSurvivors {
			continue
		}
		if a.ProjectedCostSource == "" {
			return 0, false
		}
		total += a.ProjectedCostMicro
	}
	return total, true
}
