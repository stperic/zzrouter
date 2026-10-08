package role

import (
	"testing"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stretchr/testify/assert"
)

func TestRoleFromMode(t *testing.T) {
	tests := []struct {
		name   string
		in     clusternode.Mode
		want   Role
		wantOK bool
	}{
		{"disabled", clusternode.Disabled, RoleDisabled, true},
		{"coordinator", clusternode.Coordinator, RoleCoordinator, true},
		{"unclaimed", clusternode.Unclaimed, RoleUnclaimed, true},
		{"worker", clusternode.Worker, RoleWorker, true},
		{"unknown", clusternode.Mode(99), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RoleFromMode(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}
