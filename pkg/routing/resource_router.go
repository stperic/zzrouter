// Package routing provides resource-aware model routing for zzRouter.
// This file implements intelligent endpoint selection based on resource availability,
// priority configuration, and model memory requirements.
package routing

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ============================================================================
// ResourceAwareRouter - Intelligent endpoint selection based on resources
// ============================================================================

// ResourceAwareRouter selects endpoints based on resource availability and priority
type ResourceAwareRouter struct {
	mu sync.RWMutex

	// Configuration
	routingConfig *config.RoutingConfig
	thresholds    *config.ResourceThresholds

	// Cluster-wide resource metrics (node -> metrics)
	nodeMetrics map[string]*mesh.ResourceMetrics

	// Cached priorities per format
	formatPriorities map[string][]config.NodePriority

	// Model-specific routing rules
	modelRouting map[string]*config.ModelRoutingRule

	// Local node name
	nodeName string
}

// CandidateEndpoint represents a potential endpoint with resource info
type CandidateEndpoint struct {
	// Endpoint information
	NodeName string
	AppName  string
	URL      string
	IsLocal  bool

	// Priority from config (higher = preferred)
	Priority int

	// Resource metrics
	Metrics *mesh.ResourceMetrics

	// Memory requirements
	RequiredMemoryMB int64
	HasSufficientMem bool

	// Score for ranking (computed)
	Score float64
}

// RoutingDecision represents the outcome of a routing decision
type RoutingDecision struct {
	// Selected endpoint
	SelectedNode string
	SelectedApp  string
	SelectedURL  string
	IsLocal      bool

	// Decision metadata
	DecisionReason string
	AllCandidates  []*CandidateEndpoint
	RejectedCount  int

	// Model info
	ModelName        string
	RequiredMemoryMB int64
	EstimatedFormat  string
}

// NewResourceAwareRouter creates a new resource-aware router
func NewResourceAwareRouter(nodeName string, routingConfig *config.RoutingConfig) *ResourceAwareRouter {
	router := &ResourceAwareRouter{
		nodeName:         nodeName,
		routingConfig:    routingConfig,
		nodeMetrics:      make(map[string]*mesh.ResourceMetrics),
		formatPriorities: make(map[string][]config.NodePriority),
		modelRouting:     make(map[string]*config.ModelRoutingRule),
	}

	// Initialize from config
	if routingConfig != nil {
		router.thresholds = &routingConfig.ResourceThresholds

		// Copy format priorities
		maps.Copy(router.formatPriorities, routingConfig.FormatPriorities)

		// Copy model routing rules
		for model, rule := range routingConfig.ModelRouting {
			ruleCopy := rule // Create copy
			router.modelRouting[model] = &ruleCopy
		}
	} else {
		router.thresholds = &config.ResourceThresholds{}
	}

	return router
}

// UpdateNodeMetrics updates resource metrics for a specific node
func (r *ResourceAwareRouter) UpdateNodeMetrics(nodeName string, metrics *mesh.ResourceMetrics) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodeMetrics[nodeName] = metrics
	utils.LogDebugf("[ResourceRouter] Updated metrics for node %s: GPU=%dMB free, RAM=%dMB available",
		nodeName, metrics.GPUMemoryFreeMB, metrics.RAMAvailableMB)
}

// routingSnapshot holds an immutable copy of the router's mutable state.
// Taken once per SelectEndpoint call so routing logic runs lock-free.
type routingSnapshot struct {
	nodeMetrics      map[string]*mesh.ResourceMetrics
	formatPriorities map[string][]config.NodePriority
	modelRouting     map[string]*config.ModelRoutingRule
	thresholds       *config.ResourceThresholds
	routingMode      string
}

// snapshot takes a point-in-time copy of mutable state under RLock.
func (r *ResourceAwareRouter) snapshot() routingSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snap := routingSnapshot{
		nodeMetrics:      make(map[string]*mesh.ResourceMetrics, len(r.nodeMetrics)),
		formatPriorities: r.formatPriorities, // read-only after init, safe to share
		modelRouting:     r.modelRouting,     // read-only after init, safe to share
		thresholds:       r.thresholds,
		routingMode:      r.getRoutingModeLocked(),
	}
	// Copy metrics map (pointers to immutable structs)
	maps.Copy(snap.nodeMetrics, r.nodeMetrics)
	return snap
}

// SelectEndpoint selects the best endpoint for running a model.
// Takes a snapshot of mutable state under RLock, then runs all routing
// logic lock-free to avoid contention and nested-lock deadlocks.
func (r *ResourceAwareRouter) SelectEndpoint(
	ctx context.Context,
	modelName string,
	format string,
	candidates []CandidateEndpoint,
) (*RoutingDecision, error) {
	snap := r.snapshot()

	decision := &RoutingDecision{
		ModelName:       modelName,
		EstimatedFormat: format,
		AllCandidates:   make([]*CandidateEndpoint, len(candidates)),
	}

	// Copy candidates and add metadata
	for i := range candidates {
		c := candidates[i]
		decision.AllCandidates[i] = &c
	}

	// Check for locked routing first
	lockedNode := getLockedNode(&snap, modelName)
	if lockedNode != "" {
		decision.DecisionReason = fmt.Sprintf("model locked to node: %s", lockedNode)

		// Find the locked node in candidates
		for _, c := range decision.AllCandidates {
			if c.NodeName == lockedNode {
				decision.SelectedNode = c.NodeName
				decision.SelectedApp = c.AppName
				decision.SelectedURL = c.URL
				decision.IsLocal = c.IsLocal
				return decision, nil
			}
		}

		// Locked node not available
		return nil, &RoutingError{
			Code:      503,
			Message:   "Locked node unavailable",
			Details:   fmt.Sprintf("Model %s is locked to node %s which is not available", modelName, lockedNode),
			Component: "resource_router",
			Operation: "select_endpoint",
		}
	}

	// Estimate memory requirements
	estimate := modelregistry.EstimateModelMemory(modelName, format, "")
	decision.RequiredMemoryMB = estimate.RecommendedMemoryMB

	// Enrich candidates with resource info and priority (lock-free, uses snapshot)
	enrichCandidates(&snap, decision.AllCandidates, modelName, format, estimate.RecommendedMemoryMB)

	switch snap.routingMode {
	case "priority":
		return r.selectByPriority(decision)
	case "round-robin":
		return r.selectRoundRobin(decision)
	default: // "auto" - resource-aware selection
		return r.selectByResources(&snap, decision)
	}
}

// selectByResources implements the main resource-aware routing algorithm.
//
// TOCTOU note: There is an inherent race between checking resource availability
// here and the request actually reaching the selected worker. By the time the
// request arrives, resources may have been consumed by another request. This is
// mitigated by three mechanisms:
//   - Workers reject requests they cannot serve (backpressure), causing the
//     coordinator to retry on a different node.
//   - Periodic metric updates keep the cached resource data relatively fresh.
//   - The coordinator retries failed routing attempts on alternate candidates.
func (r *ResourceAwareRouter) selectByResources(snap *routingSnapshot, decision *RoutingDecision) (*RoutingDecision, error) {
	candidates := decision.AllCandidates

	// Step 1: Filter by resource availability
	available := make([]*CandidateEndpoint, 0)
	for _, c := range candidates {
		if hasAvailableResources(snap.thresholds, c, decision.RequiredMemoryMB) {
			c.HasSufficientMem = true
			available = append(available, c)
		} else {
			decision.RejectedCount++
		}
	}

	if len(available) == 0 {
		// Check if fallback is enabled
		rule := getModelRoutingRule(snap, decision.ModelName)
		if rule != nil && !rule.FallbackEnabled {
			return nil, &RoutingError{
				Code:      503,
				Message:   "Insufficient resources",
				Details:   fmt.Sprintf("No nodes have sufficient resources for model %s (requires ~%dMB)", decision.ModelName, decision.RequiredMemoryMB),
				Component: "resource_router",
				Operation: "select_endpoint",
			}
		}

		// Fallback: try all candidates even without sufficient resources
		utils.LogDebugf("[ResourceRouter] No candidates with sufficient resources, falling back to all candidates")
		available = make([]*CandidateEndpoint, len(candidates))
		copy(available, candidates)
		decision.DecisionReason = "fallback: no nodes with sufficient resources"
	}

	// Step 2: Calculate scores for ranking
	for _, c := range available {
		c.Score = calculateScore(c, decision.RequiredMemoryMB)
	}

	// Step 3: Sort by score (descending)
	sort.Slice(available, func(i, j int) bool {
		// Higher score wins
		if available[i].Score != available[j].Score {
			return available[i].Score > available[j].Score
		}
		// Tie-breaker: more free memory
		if available[i].Metrics != nil && available[j].Metrics != nil {
			return available[i].Metrics.GPUMemoryFreeMB > available[j].Metrics.GPUMemoryFreeMB
		}
		// Final tie-breaker: higher priority value = preferred
		return available[i].Priority > available[j].Priority
	})

	// Step 4: Select the best candidate
	if len(available) > 0 {
		best := available[0]
		decision.SelectedNode = best.NodeName
		decision.SelectedApp = best.AppName
		decision.SelectedURL = best.URL
		decision.IsLocal = best.IsLocal

		if decision.DecisionReason == "" {
			decision.DecisionReason = fmt.Sprintf("resource-based selection (score=%.2f, priority=%d)", best.Score, best.Priority)
		}

		utils.LogDebugf("[ResourceRouter] Selected node %s for model %s (score=%.2f, priority=%d)",
			best.NodeName, decision.ModelName, best.Score, best.Priority)
		return decision, nil
	}

	return nil, &RoutingError{
		Code:      503,
		Message:   "No available endpoints",
		Details:   "No endpoints available to run the model",
		Component: "resource_router",
		Operation: "select_endpoint",
	}
}

// selectByPriority selects endpoint based purely on priority (ignores resources)
func (r *ResourceAwareRouter) selectByPriority(decision *RoutingDecision) (*RoutingDecision, error) {
	candidates := decision.AllCandidates

	// Sort by priority (descending — higher value = preferred)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Priority > candidates[j].Priority
	})

	if len(candidates) > 0 {
		best := candidates[0]
		decision.SelectedNode = best.NodeName
		decision.SelectedApp = best.AppName
		decision.SelectedURL = best.URL
		decision.IsLocal = best.IsLocal
		decision.DecisionReason = fmt.Sprintf("priority-only selection (priority=%d)", best.Priority)
		return decision, nil
	}

	return nil, &RoutingError{
		Code:    503,
		Message: "No available endpoints",
	}
}

// selectRoundRobin implements simple round-robin selection
func (r *ResourceAwareRouter) selectRoundRobin(decision *RoutingDecision) (*RoutingDecision, error) {
	candidates := decision.AllCandidates

	if len(candidates) == 0 {
		return nil, &RoutingError{
			Code:    503,
			Message: "No available endpoints",
		}
	}

	// Simple round-robin based on current time
	idx := int(utils.Now().UnixNano()) % len(candidates)
	selected := candidates[idx]

	decision.SelectedNode = selected.NodeName
	decision.SelectedApp = selected.AppName
	decision.SelectedURL = selected.URL
	decision.IsLocal = selected.IsLocal
	decision.DecisionReason = "round-robin selection"
	return decision, nil
}

// enrichCandidates adds resource info and priority to candidates (lock-free, uses snapshot)
func enrichCandidates(snap *routingSnapshot, candidates []*CandidateEndpoint, modelName, format string, requiredMemoryMB int64) {
	for _, c := range candidates {
		// Add resource metrics if available
		if metrics, ok := snap.nodeMetrics[c.NodeName]; ok {
			c.Metrics = metrics
		}

		// Calculate priority from config
		c.Priority = getPriorityForNode(snap, c.NodeName, c.AppName, modelName, format)
		c.RequiredMemoryMB = requiredMemoryMB
	}
}

// getPriorityForNode determines the priority for a node/app combination
func getPriorityForNode(snap *routingSnapshot, nodeName, appName, modelName, format string) int {
	// Check model-specific priorities first
	rule := getModelRoutingRule(snap, modelName)
	if rule != nil && len(rule.Priorities) > 0 {
		for _, p := range rule.Priorities {
			if matchesNodePriority(nodeName, appName, &p) {
				return p.Priority
			}
		}
	}

	// Check format-specific priorities
	if format != "" {
		if priorities, ok := snap.formatPriorities[format]; ok {
			for _, p := range priorities {
				if matchesNodePriority(nodeName, appName, &p) {
					return p.Priority
				}
			}
		}
	}

	// Default priority
	return 50
}

// matchesNodePriority checks if a node/app matches a priority rule
func matchesNodePriority(nodeName, appName string, priority *config.NodePriority) bool {
	// Node match
	if priority.Node != "*" && !strings.EqualFold(priority.Node, nodeName) {
		return false
	}

	// App match (if specified)
	if priority.App != "" && !strings.EqualFold(priority.App, appName) {
		return false
	}

	return true
}

// hasAvailableResources checks if a candidate has sufficient resources
func hasAvailableResources(thresholds *config.ResourceThresholds, candidate *CandidateEndpoint, requiredMemoryMB int64) bool {
	metrics := candidate.Metrics
	if metrics == nil {
		// No metrics available - assume available (optimistic)
		return true
	}

	// Check GPU memory if GPU is available
	if metrics.HasGPU() {
		if metrics.GPUMemoryFreeMB < requiredMemoryMB {
			return false
		}
		if metrics.GPUUtilization > thresholds.GetMaxGPUUtilization() {
			return false
		}
		if metrics.GPUMemoryFreeMB < thresholds.GetMinFreeGPUMemoryMB() {
			return false
		}
	} else {
		// CPU inference - check RAM
		if metrics.RAMAvailableMB < requiredMemoryMB {
			return false
		}
		if metrics.RAMAvailableMB < thresholds.GetMinFreeRAMMB() {
			return false
		}
	}

	return true
}

// calculateScore computes a ranking score for a candidate.
// requiredMemoryMB is plumbed for future memory-aware weighting; today the
// score is priority + load + latency + capability only.
func calculateScore(candidate *CandidateEndpoint, _ int64) float64 {
	score := 0.0

	// Priority contributes 40% (normalized to 0-40)
	score += float64(candidate.Priority) * 0.4

	// Resource availability contributes 60%
	if candidate.Metrics != nil {
		metrics := candidate.Metrics

		if metrics.HasGPU() {
			// GPU memory score (0-30): more free memory = higher score
			if metrics.GPUMemoryTotalMB > 0 {
				freeRatio := float64(metrics.GPUMemoryFreeMB) / float64(metrics.GPUMemoryTotalMB)
				score += freeRatio * 30
			}

			// GPU utilization score (0-30): lower utilization = higher score
			utilScore := (100.0 - metrics.GPUUtilization) / 100.0 * 30
			score += utilScore
		} else {
			// RAM score for CPU inference
			if metrics.RAMTotalMB > 0 {
				freeRatio := float64(metrics.RAMAvailableMB) / float64(metrics.RAMTotalMB)
				score += freeRatio * 60
			}
		}
	} else {
		// No metrics - neutral score
		score += 30
	}

	// Bonus for local execution (reduces latency)
	if candidate.IsLocal {
		score += 5
	}

	return score
}

// getLockedNode returns the node a model is locked to, or empty string
func getLockedNode(snap *routingSnapshot, modelName string) string {
	rule := getModelRoutingRule(snap, modelName)
	if rule != nil && rule.LockedTo != "" {
		return rule.LockedTo
	}
	return ""
}

// getModelRoutingRule returns the routing rule for a model (lock-free, uses snapshot)
func getModelRoutingRule(snap *routingSnapshot, modelName string) *config.ModelRoutingRule {
	// Exact match first
	if rule, ok := snap.modelRouting[modelName]; ok {
		return rule
	}

	// Try wildcard patterns
	for pattern, rule := range snap.modelRouting {
		if modelregistry.MatchesModelPattern(modelName, pattern) {
			return rule
		}
	}

	return nil
}

// getRoutingModeLocked returns the configured routing mode. Must be called with mu held.
func (r *ResourceAwareRouter) getRoutingModeLocked() string {
	if r.routingConfig != nil {
		return r.routingConfig.GetDefaultMode()
	}
	return "auto"
}

// UpdateConfig updates the routing configuration
func (r *ResourceAwareRouter) UpdateConfig(cfg *config.RoutingConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.routingConfig = cfg
	if cfg != nil {
		r.thresholds = &cfg.ResourceThresholds

		// Clear and rebuild caches
		r.formatPriorities = make(map[string][]config.NodePriority)
		maps.Copy(r.formatPriorities, cfg.FormatPriorities)

		r.modelRouting = make(map[string]*config.ModelRoutingRule)
		for model, rule := range cfg.ModelRouting {
			ruleCopy := rule
			r.modelRouting[model] = &ruleCopy
		}
	}
}
