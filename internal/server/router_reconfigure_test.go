package server

import (
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/routing"
)

// TestReconfigureRouter_FollowsWorkerBoundary verifies that when cluster mode
// transitions across the worker boundary, the active router type swaps. This
// is the runtime-mode-flip fix (design review Q4): services capture the
// Swappable once at boot, and reconfigureRouter replaces the backing impl.
func TestReconfigureRouter_FollowsWorkerBoundary(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.ClusterKey = TestClusterKey
	s := createTestNode(t, cfg)

	// Boot state: disabled → ClusterAwareRouter
	if _, ok := s.cluster.routerSwap.Current().(*routing.ClusterAwareRouter); !ok {
		t.Fatalf("boot: expected ClusterAwareRouter, got %T", s.cluster.routerSwap.Current())
	}

	// Flip to worker. reconfigureRouter should swap to LocalOnlyRouter.
	workerCfg := *s.config
	workerCfg.Cluster.SetMode(pkgConfig.ClusterModeWorker)
	s.reconfigureRouter(&workerCfg)

	if _, ok := s.cluster.routerSwap.Current().(*routing.LocalOnlyRouter); !ok {
		t.Fatalf("after worker flip: expected LocalOnlyRouter, got %T", s.cluster.routerSwap.Current())
	}

	// Flip back to disabled. Should rebuild ClusterAwareRouter.
	standaloneCfg := *s.config
	standaloneCfg.Cluster.SetMode(pkgConfig.ClusterModeDisabled)
	s.reconfigureRouter(&standaloneCfg)

	if _, ok := s.cluster.routerSwap.Current().(*routing.ClusterAwareRouter); !ok {
		t.Fatalf("after leave: expected ClusterAwareRouter, got %T", s.cluster.routerSwap.Current())
	}
}

// TestReconfigureRouter_NoOpOnSameSideOfBoundary verifies that mode transitions
// that don't cross the worker boundary (e.g., disabled ↔ coordinator) leave the
// router instance untouched — avoids needless tear-down during endpoint churn.
func TestReconfigureRouter_NoOpOnSameSideOfBoundary(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.ClusterKey = TestClusterKey
	s := createTestNode(t, cfg)

	before := s.cluster.routerSwap.Current()

	coordCfg := *s.config
	coordCfg.Cluster.SetMode(pkgConfig.ClusterModeCoordinator)
	s.reconfigureRouter(&coordCfg)

	if s.cluster.routerSwap.Current() != before {
		t.Fatalf("disabled→coordinator should not rebuild router (both are ClusterAware)")
	}
}

// TestReconfigureRouter_SubscribedToNodeConfigStore verifies the listener was
// actually registered at boot — the whole mechanism is dead without it.
func TestReconfigureRouter_SubscribedToNodeConfigStore(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.ClusterKey = TestClusterKey
	s := createTestNode(t, cfg)

	// Drive the store via the public mutation method and confirm the router
	// actually follows — this is the end-to-end wiring check.
	if _, err := s.nodeConfigStore.SetClusterJoined(); err != nil {
		t.Fatalf("SetClusterJoined: %v", err)
	}
	t.Cleanup(func() { _ = s.nodeConfigStore.SetClusterLeft() })

	if _, ok := s.cluster.routerSwap.Current().(*routing.LocalOnlyRouter); !ok {
		t.Fatalf("listener wiring: expected LocalOnlyRouter after SetClusterJoined, got %T", s.cluster.routerSwap.Current())
	}
}
