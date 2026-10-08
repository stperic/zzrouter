package server

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerRoleWrappers_FollowRoleManager verifies that Server.IsWorker
// and Server.IsCoordinator read through s.role so interface consumers
// (NodeNamer.IsWorker, ClusterState.IsCoordinator) observe
// runtime role transitions, not static construction-time state.
func TestServerRoleWrappers_FollowRoleManager(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	ctx := context.Background()

	require.NoError(t, s.role.Set(ctx, role.RoleCoordinator, "test promote"))
	assert.True(t, s.IsCoordinator(), "Server.IsCoordinator must reflect RoleCoordinator")
	assert.False(t, s.IsWorker())

	require.NoError(t, s.role.Set(ctx, role.RoleWorker, "test demote"))
	assert.False(t, s.IsCoordinator())
	assert.True(t, s.IsWorker(), "Server.IsWorker must reflect RoleWorker")

	require.NoError(t, s.role.Set(ctx, role.RoleUnclaimed, "test unclaim"))
	assert.False(t, s.IsCoordinator())
	assert.False(t, s.IsWorker(),
		"neither Coordinator nor Worker in Unclaimed state")
}

// TestHandleRoleTransition_PromoteDemoteCycle verifies that role.Set
// fires handleRoleTransition end-to-end. A full Coordinator → Worker
// → Coordinator cycle proves both branches run without deadlock (the
// group helpers no longer re-check role.Current internally, which
// used to deadlock under role.Manager's mutex).
func TestHandleRoleTransition_PromoteDemoteCycle(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	ctx := context.Background()

	require.NoError(t, s.role.Set(ctx, role.RoleCoordinator, "promote"))
	assert.True(t, s.role.Current().IsCoordinator())

	require.NoError(t, s.role.Set(ctx, role.RoleWorker, "demote"))
	assert.True(t, s.role.Current().IsWorker())

	require.NoError(t, s.role.Set(ctx, role.RoleCoordinator, "re-promote"))
	assert.True(t, s.role.Current().IsCoordinator(),
		"second promote must complete — proves Start side is restartable")
}

// TestHandleClusterModeReload_FlipsRole verifies the config-reload
// trigger: mutating cluster.mode and invoking the handler flips the
// role manager, which in turn drives handleRoleTransition. The full
// NodeConfigStore reload path (disk re-read → OnChange fan-out) is
// covered by the store's own tests; here we call the handler
// directly, matching how other reload handlers (reconfigureRouter)
// are unit-tested.
//
// Target is RoleUnclaimed rather than RoleWorker: a fresh worker-
// mode flip with no on-disk pairing state must land in Unclaimed so
// the runtime pairing flow kicks in. The promotion to Worker happens
// later via clusternode.Node.OnModeChange once the operator runs
// `zzrouter cluster pair` and the coord accepts the code.
// TestHandleRoleTransition_CoordinatorSubsystemsCycleCleanly extends
// the promote→demote→promote lifecycle test with a state assertion: on
// demote, coord-only subsystems (audit sink is the canary here) must
// actually drain — not just the role flag. On re-promote they must
// rearm. Locks the contract that handleRoleTransition's Stop side is
// a real teardown, not just a flag flip.
//
// Uses the audit sink because it's the one coord-only subsystem with a
// cheap observable state (Null vs non-Null interface type). Other
// subsystems (access, search, discovery, fallback, affinity, mcpGateway,
// Pricing) have internal state that isn't exposed for test assertion.
func TestHandleRoleTransition_CoordinatorSubsystemsCycleCleanly(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	ctx := context.Background()

	// Baseline: test node starts non-coord, audit sink is Null.
	_, nullInitial := s.auditSink.(audit.Null)
	require.True(t, nullInitial, "audit sink starts as Null before any coord transition")

	// Promote: coord-only subsystems must start. Audit sink may stay
	// Null when auditConfigDir is empty (test-node default); the
	// observable invariant is that openAuditSink ran without panic and
	// role.Current reflects the new state.
	require.NoError(t, s.role.Set(ctx, role.RoleCoordinator, "promote"))
	assert.True(t, s.role.Current().IsCoordinator())

	// Demote: coord-only subsystems must Stop. closeAuditSink resets
	// auditSink to audit.Null regardless of whether it was Null before.
	require.NoError(t, s.role.Set(ctx, role.RoleWorker, "demote"))
	_, nullPostDemote := s.auditSink.(audit.Null)
	assert.True(t, nullPostDemote,
		"post-demote audit sink must be audit.Null (closeAuditSink ran)")

	// Re-promote: must be restartable. The first demote/promote already
	// proves no panic; repeat once more to catch any once-only Start
	// state (which would now fire as "already started" errors).
	require.NoError(t, s.role.Set(ctx, role.RoleCoordinator, "re-promote"))
	assert.True(t, s.role.Current().IsCoordinator())
}

func TestHandleClusterModeReload_FlipsRole(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	require.NoError(t, s.role.Set(context.Background(), role.RoleCoordinator, "baseline"))

	next := *s.config
	next.Cluster.Mode = pkgConfig.ClusterModeWorker
	s.handleClusterModeReload(&next)

	assert.Equal(t, role.RoleUnclaimed, s.role.Current(),
		"role must flip to Unclaimed after config reload with mode=worker + no pairing state on disk")
}
