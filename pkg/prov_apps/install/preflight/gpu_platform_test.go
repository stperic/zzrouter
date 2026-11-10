package preflight

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stretchr/testify/require"
)

func TestGPURequirementPlatformVendor(t *testing.T) {
	req := &config.GPURequirement{Vendor: "nvidia", Policy: "required", Platforms: map[string]string{"darwin": "apple"}}
	for _, tc := range []struct {
		os   string
		want gpu.Vendor
	}{{"darwin", gpu.VendorApple}, {"linux", gpu.VendorNVIDIA}, {"windows", gpu.VendorNVIDIA}} {
		vendor, err := requirementVendor(req, tc.os)
		require.NoError(t, err)
		require.Equal(t, tc.want, vendor)
	}
	require.Equal(t, "nvidia", req.Vendor)
	require.Equal(t, "required", req.Policy)
	require.Equal(t, map[string]string{"darwin": "apple"}, req.Platforms)
}

func TestGPURequirementRejectsInactiveInvalidPlatform(t *testing.T) {
	for _, platforms := range []map[string]string{{"macos": "apple"}, {"darwin": ""}, {"darwin": "unknown"}} {
		req := &config.GPURequirement{Vendor: "nvidia", Policy: "required", Platforms: platforms}
		_, err := requirementVendor(req, "linux")
		require.Error(t, err)
		result, checked := GPU(t.Context(), req)
		require.True(t, checked)
		require.False(t, result.Passed)
	}
}
