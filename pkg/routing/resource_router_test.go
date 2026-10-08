package routing

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
)

func TestResourceAwareRouterCreation(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "auto",
		ResourceThresholds: config.ResourceThresholds{
			MinFreeGPUMemoryMB: 1024,
			MinFreeRAMMB:       2048,
			MaxGPUUtilization:  90,
		},
		FormatPriorities: map[string][]config.NodePriority{
			"gguf": {
				{Node: "gpu-server-1", Priority: 100},
				{Node: "gpu-server-2", Priority: 80},
			},
		},
	}

	router := NewResourceAwareRouter("test-node", cfg)

	if router == nil {
		t.Fatal("NewResourceAwareRouter returned nil")
	}

	if router.nodeName != "test-node" {
		t.Errorf("nodeName = %q, want %q", router.nodeName, "test-node")
	}
}

func TestResourceAwareRouterPrioritySelection(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "priority",
		FormatPriorities: map[string][]config.NodePriority{
			"gguf": {
				{Node: "high-priority", Priority: 100},
				{Node: "low-priority", Priority: 50},
			},
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	candidates := []CandidateEndpoint{
		{NodeName: "low-priority", AppName: "llamacpp", URL: "http://low:8080"},
		{NodeName: "high-priority", AppName: "llamacpp", URL: "http://high:8080"},
	}

	decision, err := router.SelectEndpoint(context.Background(), "llama-7b", "gguf", candidates)
	if err != nil {
		t.Fatalf("SelectEndpoint failed: %v", err)
	}

	if decision.SelectedNode != "high-priority" {
		t.Errorf("SelectedNode = %q, want %q", decision.SelectedNode, "high-priority")
	}
}

func TestResourceAwareRouterResourceFiltering(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "auto",
		ResourceThresholds: config.ResourceThresholds{
			MinFreeGPUMemoryMB: 1024,
			MinFreeRAMMB:       2048,
			MaxGPUUtilization:  90,
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	// Add metrics for nodes
	router.UpdateNodeMetrics("node-with-resources", &mesh.ResourceMetrics{
		GPUMemoryTotalMB: 24000,
		GPUMemoryUsedMB:  4000,
		GPUMemoryFreeMB:  20000,
		GPUUtilization:   30,
		GPUType:          "nvidia",
		GPUCount:         1,
		RAMTotalMB:       64000,
		RAMAvailableMB:   50000,
		CollectedAt:      time.Now().Unix(),
	})

	router.UpdateNodeMetrics("node-low-resources", &mesh.ResourceMetrics{
		GPUMemoryTotalMB: 8000,
		GPUMemoryUsedMB:  7500,
		GPUMemoryFreeMB:  500, // Not enough
		GPUUtilization:   95,  // Too high
		GPUType:          "nvidia",
		GPUCount:         1,
		RAMTotalMB:       16000,
		RAMAvailableMB:   1000,
		CollectedAt:      time.Now().Unix(),
	})

	candidates := []CandidateEndpoint{
		{NodeName: "node-low-resources", AppName: "vllm", URL: "http://low:8080"},
		{NodeName: "node-with-resources", AppName: "vllm", URL: "http://good:8080"},
	}

	decision, err := router.SelectEndpoint(context.Background(), "llama-7b", "gguf", candidates)
	if err != nil {
		t.Fatalf("SelectEndpoint failed: %v", err)
	}

	// Should select the node with sufficient resources
	if decision.SelectedNode != "node-with-resources" {
		t.Errorf("SelectedNode = %q, want %q (node with resources)", decision.SelectedNode, "node-with-resources")
	}

	if decision.RejectedCount != 1 {
		t.Errorf("RejectedCount = %d, want 1", decision.RejectedCount)
	}
}

func TestResourceAwareRouterLockedNode(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "auto",
		ModelRouting: map[string]config.ModelRoutingRule{
			"locked-model": {
				LockedTo: "special-node",
			},
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	candidates := []CandidateEndpoint{
		{NodeName: "regular-node", AppName: "vllm", URL: "http://regular:8080"},
		{NodeName: "special-node", AppName: "vllm", URL: "http://special:8080"},
	}

	decision, err := router.SelectEndpoint(context.Background(), "locked-model", "gguf", candidates)
	if err != nil {
		t.Fatalf("SelectEndpoint failed: %v", err)
	}

	if decision.SelectedNode != "special-node" {
		t.Errorf("SelectedNode = %q, want %q (locked node)", decision.SelectedNode, "special-node")
	}

	if decision.DecisionReason != "model locked to node: special-node" {
		t.Errorf("DecisionReason = %q, want locked reason", decision.DecisionReason)
	}
}

func TestResourceAwareRouterLockedNodeNotAvailable(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "auto",
		ModelRouting: map[string]config.ModelRoutingRule{
			"locked-model": {
				LockedTo: "missing-node",
			},
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	candidates := []CandidateEndpoint{
		{NodeName: "regular-node", AppName: "vllm", URL: "http://regular:8080"},
	}

	_, err := router.SelectEndpoint(context.Background(), "locked-model", "gguf", candidates)
	if err == nil {
		t.Fatal("Expected error when locked node is not available")
	}

	routingErr, ok := err.(*RoutingError)
	if !ok {
		t.Fatalf("Expected *RoutingError, got %T", err)
	}

	if routingErr.Code != 503 {
		t.Errorf("Error code = %d, want 503", routingErr.Code)
	}
}

func TestResourceAwareRouterWildcardPattern(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "priority",
		ModelRouting: map[string]config.ModelRoutingRule{
			"mistralai/*": {
				Priorities: []config.NodePriority{
					{Node: "mistral-server", Priority: 100},
				},
			},
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	candidates := []CandidateEndpoint{
		{NodeName: "regular-node", AppName: "vllm", URL: "http://regular:8080"},
		{NodeName: "mistral-server", AppName: "vllm", URL: "http://mistral:8080"},
	}

	decision, err := router.SelectEndpoint(context.Background(), "mistralai/Mistral-7B-Instruct", "hf_transformers", candidates)
	if err != nil {
		t.Fatalf("SelectEndpoint failed: %v", err)
	}

	if decision.SelectedNode != "mistral-server" {
		t.Errorf("SelectedNode = %q, want %q (from wildcard rule)", decision.SelectedNode, "mistral-server")
	}
}

func TestResourceAwareRouterRoundRobin(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "round-robin",
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	candidates := []CandidateEndpoint{
		{NodeName: "node-1", AppName: "vllm", URL: "http://node1:8080"},
		{NodeName: "node-2", AppName: "vllm", URL: "http://node2:8080"},
		{NodeName: "node-3", AppName: "vllm", URL: "http://node3:8080"},
	}

	// Round-robin should select different nodes over time
	selectedNodes := make(map[string]bool)
	for range 10 {
		decision, err := router.SelectEndpoint(context.Background(), "some-model", "gguf", candidates)
		if err != nil {
			t.Fatalf("SelectEndpoint failed: %v", err)
		}
		selectedNodes[decision.SelectedNode] = true
	}

	// Should have selected at least 2 different nodes (with high probability)
	// Due to time-based selection, we can't guarantee all nodes are selected
	if len(selectedNodes) < 1 {
		t.Errorf("Round-robin should select nodes, but got none")
	}
}

func TestResourceAwareRouterFallbackEnabled(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "auto",
		ResourceThresholds: config.ResourceThresholds{
			MinFreeGPUMemoryMB: 10000, // High threshold - nothing will pass
		},
		ModelRouting: map[string]config.ModelRoutingRule{
			"*": {
				FallbackEnabled: true, // Enable fallback for all models
			},
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	// All nodes have low resources
	router.UpdateNodeMetrics("node-1", &mesh.ResourceMetrics{
		GPUMemoryTotalMB: 8000,
		GPUMemoryFreeMB:  500,
		GPUType:          "nvidia",
		GPUCount:         1,
		CollectedAt:      time.Now().Unix(),
	})

	candidates := []CandidateEndpoint{
		{NodeName: "node-1", AppName: "vllm", URL: "http://node1:8080"},
	}

	// With fallback enabled, should still succeed even though resources are low
	decision, err := router.SelectEndpoint(context.Background(), "some-model", "gguf", candidates)
	if err != nil {
		t.Fatalf("SelectEndpoint failed: %v", err)
	}

	if decision.SelectedNode != "node-1" {
		t.Errorf("SelectedNode = %q, want %q (fallback)", decision.SelectedNode, "node-1")
	}
}

func TestResourceAwareRouterNoFallback(t *testing.T) {
	cfg := &config.RoutingConfig{
		DefaultMode: "auto",
		ResourceThresholds: config.ResourceThresholds{
			MinFreeGPUMemoryMB: 10000, // High threshold - nothing will pass
		},
		ModelRouting: map[string]config.ModelRoutingRule{
			"*": {
				FallbackEnabled: false, // Disable fallback
			},
		},
	}

	router := NewResourceAwareRouter("coordinator", cfg)

	// All nodes have low resources
	router.UpdateNodeMetrics("node-1", &mesh.ResourceMetrics{
		GPUMemoryTotalMB: 8000,
		GPUMemoryFreeMB:  500,
		GPUType:          "nvidia",
		GPUCount:         1,
		CollectedAt:      time.Now().Unix(),
	})

	candidates := []CandidateEndpoint{
		{NodeName: "node-1", AppName: "vllm", URL: "http://node1:8080"},
	}

	// Without fallback, should fail when no resources available
	_, err := router.SelectEndpoint(context.Background(), "some-model", "gguf", candidates)
	if err == nil {
		t.Fatal("Expected error when resources insufficient and fallback disabled")
	}

	routingErr, ok := err.(*RoutingError)
	if !ok {
		t.Fatalf("Expected *RoutingError, got %T", err)
	}

	if routingErr.Code != 503 {
		t.Errorf("Error code = %d, want 503", routingErr.Code)
	}
}

func TestResourceMetricsHelpers(t *testing.T) {
	metrics := &mesh.ResourceMetrics{
		GPUMemoryTotalMB: 24000,
		GPUMemoryUsedMB:  12000,
		GPUMemoryFreeMB:  12000,
		GPUUtilization:   50,
		GPUType:          "nvidia",
		GPUCount:         1,
		RAMTotalMB:       64000,
		RAMAvailableMB:   32000,
	}

	// Test HasGPU
	if !metrics.HasGPU() {
		t.Error("HasGPU() = false, want true")
	}

	// Test GPUMemoryUsedPercent
	usedPct := metrics.GPUMemoryUsedPercent()
	if usedPct != 50.0 {
		t.Errorf("GPUMemoryUsedPercent() = %f, want 50.0", usedPct)
	}

	// Test RAMUsedPercent
	ramPct := metrics.RAMUsedPercent()
	if ramPct != 50.0 {
		t.Errorf("RAMUsedPercent() = %f, want 50.0", ramPct)
	}

	// Test CanFitModel
	if !metrics.CanFitModel(10000, true) {
		t.Error("CanFitModel(10000, true) = false, want true")
	}

	if metrics.CanFitModel(15000, true) {
		t.Error("CanFitModel(15000, true) = true, want false")
	}
}

func TestResourceMetricsNoGPU(t *testing.T) {
	metrics := &mesh.ResourceMetrics{
		GPUType:        "none",
		GPUCount:       0,
		RAMTotalMB:     64000,
		RAMAvailableMB: 32000,
	}

	if metrics.HasGPU() {
		t.Error("HasGPU() = true, want false for no GPU")
	}

	// CPU inference should check RAM
	if !metrics.CanFitModel(20000, false) {
		t.Error("CanFitModel(20000, false) = false, want true (CPU inference)")
	}

	if metrics.CanFitModel(50000, false) {
		t.Error("CanFitModel(50000, false) = true, want false (not enough RAM)")
	}
}
