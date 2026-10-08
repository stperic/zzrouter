package fallback

import (
	"slices"
	"time"
)

// SkipReason is a closed-enum tag for why a candidate was filtered out
// without ever being tried. Stamped on the routing metadata so agents
// see which replicas they "lost" and why.
type SkipReason string

const (
	SkipReasonCooldown         SkipReason = "cooldown"
	SkipReasonProviderCooldown SkipReason = "provider_cooldown"
	SkipReasonUnhealthy        SkipReason = "unhealthy"
	SkipReasonTagMismatch      SkipReason = "tag_mismatch"
	SkipReasonExcluded         SkipReason = "excluded"
	SkipReasonLatencyBudget    SkipReason = "latency_budget"
)

// SteeringHints is the filter-side view of pkg/dispatch/steering.Hints.
// Kept as a local mirror so pkg/fallback doesn't import pkg/dispatch
// (which would create a cycle — pkg/dispatch/chain imports pkg/fallback).
// The dispatch chain copies fields across when calling FilterDeployments.
type SteeringHints struct {
	ExcludeReplicas []string
	LatencyBudgetMs int
	// RequireTags is handled by the existing requestTags parameter
	// already plumbed through FilterDeployments; it's not duplicated
	// here so the existing call site keeps working unchanged.
}

// LatencySnapshotter is the narrow read interface FilterDeployments
// needs for the Latency-Budget-Ms hint. *LatencyTracker satisfies it;
// nil is safe (the budget hint is treated as no-op).
type LatencySnapshotter interface {
	Snapshot(deploymentName string) (time.Duration, int)
}

// Skip records one candidate rejected by the pre-dispatch filter pass.
type Skip struct {
	Candidate Candidate
	Reason    SkipReason
}

// FilterDeployments applies cooldown, health, tag, and steering-hint
// filters in a single pass. Returns the survivors (preserving input
// order) and a parallel list of skip records for the rejected
// candidates. Nil managers/checkers/latency are safe (the filter is
// skipped). Empty requestTags disables tag filtering. Zero-value
// hints disable all steering filters.
//
// Filter order (first match wins so the reason is the *primary*
// cause):
//
//	cooldown → provider_cooldown → unhealthy → excluded
//	  → latency_budget → tag_mismatch
//
// Steering filters land between the engine-side filters and the
// tag filter so an excluded replica reports "excluded" rather than
// e.g. "unhealthy" if both happen to be true — the agent's hint is
// the most actionable signal.
func FilterDeployments(
	candidates []Candidate,
	cooldowns *CooldownManager,
	providerCooldowns *CooldownManager,
	health *HealthChecker,
	requestTags []string,
	latency LatencySnapshotter,
	hints SteeringHints,
) ([]Candidate, []Skip) {
	result := make([]Candidate, 0, len(candidates))
	var skipped []Skip

	excludeSet := nameSet(hints.ExcludeReplicas)
	budget := time.Duration(hints.LatencyBudgetMs) * time.Millisecond

	for _, c := range candidates {
		switch {
		case cooldowns != nil && cooldowns.InCooldown(c.Name):
			skipped = append(skipped, Skip{Candidate: c, Reason: SkipReasonCooldown})
		case providerCooldowns != nil && providerCooldowns.InCooldown(c.App):
			skipped = append(skipped, Skip{Candidate: c, Reason: SkipReasonProviderCooldown})
		case health != nil && !health.IsHealthy(c.Name):
			skipped = append(skipped, Skip{Candidate: c, Reason: SkipReasonUnhealthy})
		case excludeSet[c.Name]:
			skipped = append(skipped, Skip{Candidate: c, Reason: SkipReasonExcluded})
		case budget > 0 && exceedsLatencyBudget(latency, c.Name, budget):
			skipped = append(skipped, Skip{Candidate: c, Reason: SkipReasonLatencyBudget})
		case len(requestTags) > 0 && !matchesTags(c.Tags, requestTags):
			skipped = append(skipped, Skip{Candidate: c, Reason: SkipReasonTagMismatch})
		default:
			result = append(result, c)
		}
	}

	return result, skipped
}

// nameSet builds a quick membership lookup for the per-request
// Exclude-Replicas list. Returns nil when the input is empty so the
// caller's map lookups still work but allocations are zero.
func nameSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// exceedsLatencyBudget reports whether the named replica's tracked
// average latency is over the per-request budget. Replicas with no
// samples (Snapshot returns 0 samples) pass — fail-open on cold-start
// replicas so an unmet budget can't create a permanent dead state
// where untried replicas never accumulate evidence.
func exceedsLatencyBudget(latency LatencySnapshotter, name string, budget time.Duration) bool {
	if latency == nil {
		return false
	}
	avg, samples := latency.Snapshot(name)
	if samples == 0 {
		return false
	}
	return avg > budget
}

// matchesTags returns true if the deployment tags contain ALL requested tags.
// Uses nested loop (O(n*m)) which is faster than map allocation for typical small tag sets.
func matchesTags(deploymentTags []string, requestTags []string) bool {
	for _, rt := range requestTags {
		found := slices.Contains(deploymentTags, rt)
		if !found {
			return false
		}
	}
	return true
}
