package role

import (
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestRoleFromConfig(t *testing.T) {
	tests := []struct {
		name   string
		mode   pkgConfig.ClusterMode
		paired bool
		want   Role
	}{
		{"coordinator", pkgConfig.ClusterModeCoordinator, false, RoleCoordinator},
		{"worker paired", pkgConfig.ClusterModeWorker, true, RoleWorker},
		{"worker unpaired", pkgConfig.ClusterModeWorker, false, RoleUnclaimed},
		{"disabled", pkgConfig.ClusterModeDisabled, false, RoleCoordinator},
		{"standalone", pkgConfig.ClusterModeStandalone, false, RoleCoordinator},
		{"empty", "", false, RoleCoordinator},
		// `paired` is ignored outside worker mode — a coordinator with
		// stale worker pairing state on disk still boots as coordinator.
		{"coordinator ignores paired", pkgConfig.ClusterModeCoordinator, true, RoleCoordinator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RoleFromConfig(pkgConfig.ClusterConfig{Mode: tt.mode}, tt.paired)
			assert.Equal(t, tt.want, got)
		})
	}
}
