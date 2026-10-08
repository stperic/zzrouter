package variant

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// llamaCppFixtureVariants returns a representative set of llama.cpp variants
// mirroring the real shipped providers tree.
func llamaCppFixtureVariants() []config.AppInstallVariant {
	return []config.AppInstallVariant{
		{
			ID:        "win-amd64-cuda-12.4",
			Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
			Artifact:  "llama-{version}-bin-win-cuda-12.4-x64.zip",
			GPU:       &config.VariantGPURequirement{Vendor: "nvidia", CUDAMin: "12.0"},
		},
		{
			ID:        "win-amd64-cpu",
			Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
			Artifact:  "llama-{version}-bin-win-cpu-x64.zip",
		},
		{
			ID:        "linux-amd64",
			Platforms: []config.InstallPlatform{{OS: "linux", Arch: "amd64"}},
			Artifact:  "llama-{version}-bin-ubuntu-x64.tar.gz",
		},
		{
			ID:        "darwin-arm64",
			Platforms: []config.InstallPlatform{{OS: "darwin", Arch: "arm64"}},
			Artifact:  "llama-{version}-bin-macos-arm64.tar.gz",
		},
	}
}

func TestSelect_WindowsNvidiaPrefersCuda(t *testing.T) {
	hw := &hardware.HardwareInfo{
		GPUType:  "nvidia",
		GPUCount: 1,
		GPUs:     []hardware.GPUInfo{{Name: "NVIDIA A16-16Q", CUDAVersion: "12.4"}},
	}
	v, err := Select(
		llamaCppFixtureVariants(),
		fsroot.Platform{OS: "windows", Arch: "amd64"},
		hw,
	)
	require.NoError(t, err)
	assert.Equal(t, "win-amd64-cuda-12.4", v.ID)
	assert.Equal(t, "llama-b8705-bin-win-cuda-12.4-x64.zip", RenderArtifact(v, "b8705"))
}

func TestSelect_WindowsNoGPUFallsToCPU(t *testing.T) {
	hw := &hardware.HardwareInfo{GPUType: "none"}
	v, err := Select(
		llamaCppFixtureVariants(),
		fsroot.Platform{OS: "windows", Arch: "amd64"},
		hw,
	)
	require.NoError(t, err)
	assert.Equal(t, "win-amd64-cpu", v.ID)
}

func TestSelect_NvidiaTooOldCUDAFallsToCPU(t *testing.T) {
	// CUDA 11.0 should not satisfy cuda_min: 12.0
	hw := &hardware.HardwareInfo{
		GPUType: "nvidia",
		GPUs:    []hardware.GPUInfo{{CUDAVersion: "11.0"}},
	}
	v, err := Select(
		llamaCppFixtureVariants(),
		fsroot.Platform{OS: "windows", Arch: "amd64"},
		hw,
	)
	require.NoError(t, err)
	assert.Equal(t, "win-amd64-cpu", v.ID)
}

func TestSelect_AMDGPUOnWindowsFallsToCPU(t *testing.T) {
	// Vendor mismatch: AMD doesn't match nvidia filter, so CUDA variant is skipped.
	hw := &hardware.HardwareInfo{
		GPUType: "amd",
		GPUs:    []hardware.GPUInfo{{Name: "Radeon"}},
	}
	v, err := Select(
		llamaCppFixtureVariants(),
		fsroot.Platform{OS: "windows", Arch: "amd64"},
		hw,
	)
	require.NoError(t, err)
	assert.Equal(t, "win-amd64-cpu", v.ID)
}

func TestSelect_LinuxMatches(t *testing.T) {
	v, err := Select(
		llamaCppFixtureVariants(),
		fsroot.Platform{OS: "linux", Arch: "amd64"},
		&hardware.HardwareInfo{GPUType: "nvidia"},
	)
	require.NoError(t, err)
	assert.Equal(t, "linux-amd64", v.ID)
}

func TestSelect_UnknownPlatformErrors(t *testing.T) {
	_, err := Select(
		llamaCppFixtureVariants(),
		fsroot.Platform{OS: "freebsd", Arch: "amd64"},
		nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoMatchingVariant)
	// Wrap must preserve the platform context for logs/operators.
	assert.Contains(t, err.Error(), "freebsd/amd64")
}

func TestSelect_EmptyVariantsErrors(t *testing.T) {
	_, err := Select(nil, fsroot.Platform{OS: "linux", Arch: "amd64"}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoVariantsDeclared)
}

func TestSelect_OnlyGPUVariantsNoGPUErrors(t *testing.T) {
	variants := []config.AppInstallVariant{
		{
			ID:        "only-cuda",
			Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
			Artifact:  "cuda-only.zip",
			GPU:       &config.VariantGPURequirement{Vendor: "nvidia"},
		},
	}
	_, err := Select(
		variants,
		fsroot.Platform{OS: "windows", Arch: "amd64"},
		&hardware.HardwareInfo{GPUType: "none"},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoGPUMatch)
}

// TestRenderArtifacts_SingularBackCompat covers the legacy `artifact:` form
// where a YAML config has not yet migrated to the plural Artifacts list.
// NormalizeArtifacts must fold the singular value into Artifacts[0].
func TestRenderArtifacts_SingularBackCompat(t *testing.T) {
	v := config.AppInstallVariant{
		ID:        "win-amd64-cpu",
		Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
		Artifact:  "llama-{version}-bin-win-cpu-x64.zip",
	}
	rendered := RenderArtifacts(&v, "b8963")
	require.Len(t, rendered, 1)
	assert.Equal(t, "llama-b8963-bin-win-cpu-x64.zip", rendered[0].Filename)
	// Singular RenderArtifact still works post-fold.
	assert.Equal(t, "llama-b8963-bin-win-cpu-x64.zip", RenderArtifact(&v, "b8963"))
}

// TestRenderArtifacts_PeerArtifacts is the canonical motivating case:
// llama.cpp Windows + CUDA ships binary + cudart as peer zips of one
// release. Both must render, both go through {version} substitution
// (the cudart filename happens to be stable across releases — that is
// fine, ReplaceAll is a no-op on strings without the token).
func TestRenderArtifacts_PeerArtifacts(t *testing.T) {
	v := config.AppInstallVariant{
		ID:        "win-amd64-cuda-13.1",
		Platforms: []config.InstallPlatform{{OS: "windows", Arch: "amd64"}},
		Artifacts: []config.VariantArtifact{
			{Filename: "llama-{version}-bin-win-cuda-13.1-x64.zip"},
			{Filename: "cudart-llama-bin-win-cuda-13.1-x64.zip"},
		},
		GPU: &config.VariantGPURequirement{Vendor: "nvidia", CUDAMin: "13.0"},
	}
	rendered := RenderArtifacts(&v, "b8963")
	require.Len(t, rendered, 2)
	assert.Equal(t, "llama-b8963-bin-win-cuda-13.1-x64.zip", rendered[0].Filename)
	assert.Equal(t, "cudart-llama-bin-win-cuda-13.1-x64.zip", rendered[1].Filename)
}

// TestNormalizeArtifacts_PluralWinsOverSingular guards the rule that
// when both forms are populated, Artifacts is the source of truth and
// the singular Artifact field is ignored. The singular form is the
// back-compat affordance, never additive.
func TestNormalizeArtifacts_PluralWinsOverSingular(t *testing.T) {
	v := config.AppInstallVariant{
		Artifact:  "ignored.zip",
		Artifacts: []config.VariantArtifact{{Filename: "real.zip"}},
	}
	v.NormalizeArtifacts()
	require.Len(t, v.Artifacts, 1)
	assert.Equal(t, "real.zip", v.Artifacts[0].Filename)
}

func TestCudaAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"12.4", "12.0", true},
		{"12.0", "12.0", true},
		{"13.1", "12.0", true},
		{"11.8", "12.0", false},
		{"12.4", "12.5", false},
		{"", "12.0", false},
		{"12.4", "", true},
	}
	for _, c := range cases {
		assert.Equal(t, c.ok, cudaAtLeast(c.have, c.want),
			"cudaAtLeast(%q, %q)", c.have, c.want)
	}
}
