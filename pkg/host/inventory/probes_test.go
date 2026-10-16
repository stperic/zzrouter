package inventory

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedPoolIsCountedOnce(t *testing.T) {
	cache := &Cache{ttl: time.Hour, lastUpdate: utils.Now(), gpuCardsAt: utils.Now(),
		memInfo: &hardware.HardwareInfo{TotalRAM: 64 << 30, AvailableRAM: 32 << 30}}
	inv := gpu.Inventory{Cards: []gpu.Card{{Vendor: gpu.VendorApple}, {Vendor: gpu.VendorApple}}}
	local := GatherGPU(t.Context(), inv, cache)
	require.True(t, local.UnifiedMemory)
	assert.Equal(t, float64(64), local.MemoryTotalGB)
	require.Len(t, local.GPUs, 2)
	assert.Equal(t, local.MemoryTotalGB, local.GPUs[0].MemoryTotalGB+local.GPUs[1].MemoryTotalGB)
	assert.Equal(t, float64(32), *local.GPUs[0].MemoryAvailableGB)
	assert.Nil(t, local.GPUs[1].MemoryAvailableGB)
}
