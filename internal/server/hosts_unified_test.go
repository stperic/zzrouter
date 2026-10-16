package server

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedMemoryLocalAndLegacySnapshotAgree(t *testing.T) {
	for _, typ := range []string{"apple", "nvidia", "none"} {
		t.Run(typ, func(t *testing.T) {
			inv := gpu.Inventory{}
			if typ != "none" {
				inv.Cards = []gpu.Card{{Vendor: gpu.Vendor(typ), Name: "fixture GPU", MemoryMiB: 1024}}
			}
			local := inventory.GatherGPU(t.Context(), inv, inventory.NewCache(time.Minute))
			assert.Equal(t, typ == "apple", local.UnifiedMemory)
			snap := mesh.EndpointSnapshot{HealthReport: mesh.HealthReport{GPUType: typ, GPUCount: local.Count, VRAMTotalGB: local.MemoryTotalGB}}
			for _, detailed := range []bool{false, true} {
				if detailed {
					snap.GPUs = []mesh.GPUDetail{{MemoryTotalGB: local.MemoryTotalGB}}
				}
				peer := snapshotGPUInfo(snap)
				if typ == "none" {
					assert.Nil(t, peer)
					continue
				}
				assert.Equal(t, local.UnifiedMemory, peer["unified_memory"] == true)
				if typ == "apple" {
					require.NotZero(t, local.MemoryTotalGB)
					cards := peer["gpus"].([]map[string]any)
					require.Len(t, cards, 1)
					assert.Equal(t, local.MemoryTotalGB, cards[0]["memory_total_gb"])
				}
			}
		})
	}
}
