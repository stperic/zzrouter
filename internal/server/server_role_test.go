package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/filestore"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// requireNodeEnded checks that a from→to role change reached the node's one
// fatal path, the error Start returns.
func requireNodeEnded(t *testing.T, s *Server, from, to role.Role) {
	t.Helper()
	select {
	case err := <-s.servingErrors:
		require.ErrorContains(t, err, fmt.Sprintf("cluster role changed from %s to %s", from, to))
	default:
		t.Fatalf("a %s to %s change must end the node", from, to)
	}
}

// crossRole moves s across the coordinator boundary; the change ends the node.
func crossRole(t *testing.T, s *Server, to role.Role) {
	t.Helper()
	from := s.role.Current()
	require.NoError(t, s.role.Set(t.Context(), to, "test crossing"))
	requireNodeEnded(t, s, from, to)
}

// TestServerRoleWrappers_FollowRoleManager verifies that Server.IsWorker
// and Server.IsCoordinator read through s.role so interface consumers
// (NodeNamer.IsWorker, ClusterState.IsCoordinator) observe the runtime
// transitions a worker makes (pairing, leaving), not construction-time state.
func TestServerRoleWrappers_FollowRoleManager(t *testing.T) {
	coordinator := createTestNodeWithDefaults(t)
	assert.True(t, coordinator.IsCoordinator(), "Server.IsCoordinator must reflect RoleCoordinator")
	assert.False(t, coordinator.IsWorker())

	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeWorker})
	ctx := context.Background()
	assert.False(t, s.IsCoordinator())
	assert.False(t, s.IsWorker(), "an unpaired worker boots Unclaimed")

	require.NoError(t, s.role.Set(ctx, role.RoleWorker, "test pairing"))
	requireNodeRunning(t, s)
	assert.False(t, s.IsCoordinator())
	assert.True(t, s.IsWorker(), "Server.IsWorker must reflect RoleWorker")

	require.NoError(t, s.role.Set(ctx, role.RoleUnclaimed, "test leave"))
	requireNodeRunning(t, s)
	assert.False(t, s.IsCoordinator())
	assert.False(t, s.IsWorker(),
		"neither Coordinator nor Worker in Unclaimed state")
}

// requireNodeRunning checks that a change on one side of the coordinator
// boundary (pairing, leaving) did not reach the fatal path.
func requireNodeRunning(t *testing.T, s *Server) {
	t.Helper()
	select {
	case err := <-s.servingErrors:
		t.Fatalf("pairing and leaving stay on one side of the coordinator boundary: %v", err)
	default:
	}
}

// TestHandleRoleTransition_CrossingEndsStart runs real nodes: a role change
// across the coordinator boundary, in either direction, makes Start return
// an error naming both roles, and the stop it runs leaves no goroutine of
// either subsystem group behind, though the role no longer matches the boot.
func TestHandleRoleTransition_CrossingEndsStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode pkgConfig.ClusterMode
		to   role.Role
	}{
		{"demotion", pkgConfig.ClusterModeDisabled, role.RoleWorker},
		{"promotion", pkgConfig.ClusterModeWorker, role.RoleCoordinator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, port, err := net.SplitHostPort(browserBindAddress(t))
			require.NoError(t, err)
			cfg := TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: tc.mode}
			cfg.Port, err = strconv.Atoi(port)
			require.NoError(t, err)
			s := createTestNode(t, cfg)
			// The process-wide GPU probe owns its lifetime; no node stops it.
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent(),
				goleak.IgnoreAnyFunction("github.com/stperic/zzrouter/pkg/discovery/gpu.load"),
				goleak.IgnoreAnyFunction("github.com/stperic/zzrouter/pkg/discovery/gpu.reprobeAsync.func1"))
			from := s.role.Current()
			started := make(chan error, 1)
			go func() { started <- s.Start(context.Background()) }()
			select {
			case <-s.listenerReady:
			case err := <-started:
				t.Fatalf("Start returned before serving: %v", err)
			}
			require.NoError(t, s.role.Set(t.Context(), tc.to, "test crossing"))
			select {
			case err := <-started:
				require.ErrorContains(t, err, fmt.Sprintf("cluster role changed from %s to %s", from, tc.to))
				require.ErrorContains(t, err, "restarts the node as "+string(tc.to))
			case <-time.After(30 * time.Second):
				t.Fatal("a running node must exit when its role crosses the coordinator boundary")
			}
		})
	}
}

// TestHandleClusterModeReload_FlipsRole verifies the config-reload
// trigger: mutating cluster.mode and invoking the handler flips the
// role manager, which in turn drives handleRoleTransition. The full
// NodeConfigStore reload path (disk re-read → OnChange fan-out) is
// covered by the store's own tests; here we call the handler directly.
//
// Target is RoleUnclaimed rather than RoleWorker: a fresh worker-
// mode flip with no on-disk pairing state must land in Unclaimed so
// the runtime pairing flow kicks in. Leaving the coordinator role
// ends the node, which restarts as a worker.
func TestHandleClusterModeReload_FlipsRole(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	require.Equal(t, role.RoleCoordinator, s.role.Current())

	next := *s.config
	next.Cluster.Mode = pkgConfig.ClusterModeWorker
	s.handleClusterModeReload(&next)

	assert.Equal(t, role.RoleUnclaimed, s.role.Current(),
		"role must flip to Unclaimed after config reload with mode=worker + no pairing state on disk")
	requireNodeEnded(t, s, role.RoleCoordinator, role.RoleUnclaimed)
}

// TestHandleClusterModeReload_SubscribedToNodeConfigStore verifies the
// listener is registered at boot: joining a cluster through the store leaves
// the coordinator role, which ends the node.
func TestHandleClusterModeReload_SubscribedToNodeConfigStore(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	_, err := s.nodeConfigStore.SetClusterJoined()
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.nodeConfigStore.SetClusterLeft() })
	assert.Equal(t, role.RoleUnclaimed, s.role.Current())
	requireNodeEnded(t, s, role.RoleCoordinator, role.RoleUnclaimed)
}

// TestRoleCrossingAfterStopDoesNotBlock pins that the fatal path drops what
// arrives once shutdown began: role subscribers run under the role manager's
// lock, so a send nobody will read must not wedge it.
func TestRoleCrossingAfterStopDoesNotBlock(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	require.NoError(t, s.Stop())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, to := range []role.Role{role.RoleWorker, role.RoleCoordinator, role.RoleWorker} {
			_ = s.role.Set(context.Background(), to, "late crossing")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a role crossing after stop blocked the role manager")
	}
}

// TestWorkerOpensNoAccessStore: a node that boots as a worker reads neither
// keys.yaml nor teams.yaml, creates no access directory (so no people lock),
// and authenticates static installation keys only.
func TestWorkerOpensNoAccessStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, dir string) string // returns a virtual key's raw secret, if any
	}{
		{"virtual admin key on disk", func(t *testing.T, dir string) string {
			store, err := filestore.Open(t.Context(), dir, filestore.Options{})
			require.NoError(t, err)
			_, err = store.CreateTeam(t.Context(), "operators", teams.Team{Name: "Operators", Kind: teams.KindShared}, audit.Event{})
			require.NoError(t, err)
			raw := seedTestKey(t, store, "worker-admin", keys.VirtualKey{Name: "Worker admin", Role: "admin", TeamID: "operators", TeamRole: "owner"})
			require.NoError(t, store.Close())
			return raw
		}},
		{"unparseable records", func(t *testing.T, dir string) string {
			for _, name := range []string{"keys.yaml", "teams.yaml"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{unparseable"), 0o600))
			}
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dir, raw string
			s := createSeededTestNode(t, seededTestNodeConfig{
				TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeWorker},
				prepareProviders: func(string) {
					dir = pkgConfig.NewConfigManager("zzrouter").GetNodeConfigDir()
					require.NoError(t, os.MkdirAll(dir, 0o700))
					raw = tc.seed(t, dir)
				},
			})
			reset := func(key string) int {
				return makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/cluster/reset", key, nil).Code
			}
			require.Equal(t, http.StatusOK, reset(TestAdminKey), "the static admin key authenticates on a worker")
			if raw != "" {
				require.Equal(t, http.StatusUnauthorized, reset(raw), "a virtual key never authenticates on a worker")
			}
			// Guards the next change, whose database lives in access/; the old worker never created it either.
			require.NoDirExists(t, filepath.Join(dir, "access"))
		})
	}
}
