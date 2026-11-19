package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// useTestCluster makes cluster s's mesh coordinator: the one s started stops
// now, so no health monitor outlives the test, and cluster stops at cleanup.
func useTestCluster(t *testing.T, s *Server, cluster *mesh.Cluster) {
	t.Helper()
	if s.cluster.coordinator != nil {
		s.cluster.coordinator.Stop()
	}
	s.cluster.coordinator = cluster
	t.Cleanup(cluster.Stop)
}
