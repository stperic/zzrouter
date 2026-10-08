// Package variant selects an install variant (platform + optional GPU filter)
// from the list declared in provider config. It's intentionally decoupled from
// the rest of the install machinery: the only install-side type it touches is
// fsroot.Platform, and its sentinels are raised here (root re-exports them
// for backward-compatible errors.Is matches).
package variant

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// Sentinels raised by Select. Root re-exports these so callers that
// import pkg/prov_apps/install keep matching with errors.Is.
var (
	ErrNoVariantsDeclared = errors.New("no install_variants declared")
	ErrNoMatchingVariant  = errors.New("no install_variants matches target platform")
	ErrNoGPUMatch         = errors.New("only GPU-gated install_variants declared and host does not satisfy any")
)

// Select picks the best-matching AppInstallVariant for the target platform
// and hardware. Selection order:
//
//  1. Drop variants whose Platforms list does not contain the target platform.
//  2. Return the first remaining variant whose GPU filter matches the target
//     hardware. GPU-aware variants are expected to be listed before CPU-only
//     variants in provider config.
//  3. Otherwise return the first remaining variant with no GPU filter
//     (CPU / generic build).
//  4. If nothing matches, return an error. There is NO hardcoded fallback.
//
// Callers should load variants from provider config and pass the target node's
// detected hardware. When installing locally this is the result of calling
// DetectLocalHardware().
func Select(
	variants []config.AppInstallVariant,
	target fsroot.Platform,
	hw *hardware.HardwareInfo,
) (*config.AppInstallVariant, error) {
	if len(variants) == 0 {
		return nil, ErrNoVariantsDeclared
	}

	// Fold any back-compat singular Artifact into Artifacts[0] before
	// selection so callers downstream see a uniform slice regardless of
	// which YAML form was used.
	for i := range variants {
		variants[i].NormalizeArtifacts()
	}

	var candidates []*config.AppInstallVariant
	for i := range variants {
		v := &variants[i]
		if platformMatches(v.Platforms, target) {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: %s/%s", ErrNoMatchingVariant, target.OS, target.Arch)
	}

	// First pass: prefer GPU-matching variants.
	for _, v := range candidates {
		if v.GPU == nil {
			continue
		}
		if gpuMatches(v.GPU, hw) {
			return v, nil
		}
	}

	// Second pass: first CPU / generic (no GPU filter) variant.
	for _, v := range candidates {
		if v.GPU == nil {
			return v, nil
		}
	}

	// Only GPU-gated variants exist for this platform and none match.
	gpuDesc := "no GPU detected"
	if hw != nil && hw.GPUType != "" && hw.GPUType != "none" {
		gpuDesc = fmt.Sprintf("GPU=%s", hw.GPUType)
		if len(hw.GPUs) > 0 && hw.GPUs[0].CUDAVersion != "" {
			gpuDesc += fmt.Sprintf(" cuda=%s", hw.GPUs[0].CUDAVersion)
		}
	}
	return nil, fmt.Errorf("%w: platform %s/%s, host %s", ErrNoGPUMatch, target.OS, target.Arch, gpuDesc)
}

// platformMatches returns true if target is listed in platforms.
func platformMatches(platforms []config.InstallPlatform, target fsroot.Platform) bool {
	for _, p := range platforms {
		if strings.EqualFold(p.OS, target.OS) && strings.EqualFold(p.Arch, target.Arch) {
			return true
		}
	}
	return false
}

// gpuMatches evaluates a variant's GPU filter against the target hardware.
// A nil req matches any hardware; a nil hw never matches a non-nil req.
func gpuMatches(req *config.VariantGPURequirement, hw *hardware.HardwareInfo) bool {
	if req == nil {
		return true
	}
	if hw == nil || hw.GPUType == "" || hw.GPUType == "none" {
		return false
	}
	if req.Vendor != "" && !strings.EqualFold(req.Vendor, hw.GPUType) {
		return false
	}
	if req.CUDAMin != "" {
		// CUDAVersion is the driver's max-supported CUDA runtime —
		// a node-wide value stamped on every NVIDIA card by the gpu
		// inventory; non-NVIDIA cards keep the field empty. Scan for
		// the first non-empty CUDAVersion instead of indexing GPUs[0]:
		// on a mixed AMD+NVIDIA node GPUs[0] may be the AMD card
		// (which has no CUDAVersion), so the index-based read would
		// spuriously fail the filter.
		cuda := firstNonEmptyCUDAVersion(hw.GPUs)
		if cuda == "" {
			return false
		}
		if !cudaAtLeast(cuda, req.CUDAMin) {
			return false
		}
	}
	return true
}

// firstNonEmptyCUDAVersion scans the card slice and returns the
// first populated CUDAVersion. Only the NVIDIA inventory path
// writes this field, so a non-empty value is unambiguously the
// NVIDIA driver's max-supported CUDA runtime.
func firstNonEmptyCUDAVersion(gpus []hardware.GPUInfo) string {
	for _, g := range gpus {
		if g.CUDAVersion != "" {
			return g.CUDAVersion
		}
	}
	return ""
}

// cudaAtLeast returns true if have >= want. Both are dotted decimal version
// strings (e.g. "12.4"). Missing have returns false. Missing want returns true.
// Only major.minor are compared; anything beyond is ignored.
func cudaAtLeast(have, want string) bool {
	if want == "" {
		return true
	}
	if have == "" {
		return false
	}
	ha, hi := splitMajorMinor(have)
	wa, wi := splitMajorMinor(want)
	if ha != wa {
		return ha > wa
	}
	return hi >= wi
}

func splitMajorMinor(s string) (int, int) {
	parts := strings.Split(s, ".")
	maj, _ := strconv.Atoi(parts[0])
	min := 0
	if len(parts) > 1 {
		min, _ = strconv.Atoi(parts[1])
	}
	return maj, min
}

// RenderArtifact substitutes {version} in a variant's primary artifact.
// Returns the rendered filename of Artifacts[0] (after NormalizeArtifacts
// folds the back-compat singular form). Panics if the variant has no
// artifacts — Select guarantees normalized variants reach here, and a
// variant with zero artifacts would have failed validation upstream.
//
// Prefer RenderArtifacts for new call-sites; this helper is retained for
// installers that still treat the variant as a single-archive shape.
func RenderArtifact(v *config.AppInstallVariant, version string) string {
	v.NormalizeArtifacts()
	if len(v.Artifacts) == 0 {
		return ""
	}
	return strings.ReplaceAll(v.Artifacts[0].Filename, "{version}", version)
}

// RenderedArtifact is one fully-resolved (filename, url-pattern) pair.
// URLPattern is the per-artifact override if set, otherwise empty — the
// installer applies its own release-host convention when URLPattern == "".
type RenderedArtifact struct {
	Filename   string
	URLPattern string
}

// RenderArtifacts returns every artifact for the variant with {version}
// substituted in the filename. Single-artifact variants return a one-
// element slice. Empty variants return nil; callers should treat that as
// a fatal config error (variant.Select normalizes before returning, so an
// empty Artifacts slice on a returned variant means the YAML declared
// neither Artifact nor Artifacts).
func RenderArtifacts(v *config.AppInstallVariant, version string) []RenderedArtifact {
	v.NormalizeArtifacts()
	if len(v.Artifacts) == 0 {
		return nil
	}
	out := make([]RenderedArtifact, len(v.Artifacts))
	for i, a := range v.Artifacts {
		out[i] = RenderedArtifact{
			Filename:   strings.ReplaceAll(a.Filename, "{version}", version),
			URLPattern: a.URLPattern,
		}
	}
	return out
}

// DetectLocalHardware returns the current host's hardware info for variant
// selection. Safe to call repeatedly; each call re-runs discovery so a GPU
// driver install between calls is reflected.
func DetectLocalHardware(ctx context.Context) *hardware.HardwareInfo {
	hw, _ := hardware.DiscoverGPUs(ctx)
	return &hw
}
