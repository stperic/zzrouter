//go:build darwin

package gpu

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// probePlatform on macOS answers only two questions:
//
//  1. Is this machine Apple Silicon? (VendorApple → StateDriverOK
//     with the chip model as Name; the Metal/MLX stack is always
//     present on macOS, so there is no "no driver" state.)
//  2. Does it have a discrete NVIDIA/AMD GPU? In practice modern
//     macOS no longer ships drivers for either, so we return
//     StateAbsent for both — there is no path to an install hint.
//
// Callers that want a GPU on macOS should be querying VendorApple.
func probePlatform(ctx context.Context, vendor Vendor) (Detection, error) {
	switch vendor {
	case VendorApple:
		return probeAppleSilicon(ctx), nil
	case VendorNVIDIA, VendorAMD:
		// macOS has no driver for either; Intel Macs that still
		// boot on old OS versions are out of scope.
		return Detection{Vendor: vendor, State: StateAbsent}, nil
	default:
		return Detection{Vendor: vendor, State: StateUnknown},
			fmt.Errorf("gpu: unsupported vendor %q", vendor)
	}
}

// appleChipPattern matches the CPU brand string produced by
// sysctl machdep.cpu.brand_string on an Apple Silicon machine,
// e.g. "Apple M1 Pro", "Apple M3 Max".
var appleChipPattern = regexp.MustCompile(
	`(?i)apple\s+m[1-9]\d*(?:\s+(?:pro|max|ultra))?`,
)

func probeAppleSilicon(ctx context.Context) Detection {
	d := Detection{Vendor: VendorApple, State: StateAbsent}
	if runtime.GOARCH != "arm64" {
		return d
	}
	brand, err := readAppleCPUBrand(ctx)
	if err != nil {
		d.Diagnostic = err.Error()
		return d
	}
	match := appleChipPattern.FindString(brand)
	if match == "" {
		return d
	}
	d.State = StateDriverOK
	d.Name = match
	d.Count = 1 // Apple Silicon GPUs are always 1 per package.
	return d
}

// readAppleCPUBrand returns the value of the
// machdep.cpu.brand_string sysctl — e.g. "Apple M3 Pro" — without
// shelling out to /usr/sbin/sysctl. Going through unix.Sysctl
// avoids a subprocess on every probe and keeps the call testable
// (no PATH dependency, no exec.LookPath cost). ctx is accepted
// for symmetry with the other probes; the underlying syscall is
// instantaneous and not cancellable.
func readAppleCPUBrand(ctx context.Context) (string, error) {
	_ = ctx // sysctl is a single non-blocking syscall; nothing to cancel.
	v, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return "", fmt.Errorf("sysctl machdep.cpu.brand_string: %w", err)
	}
	return strings.TrimSpace(v), nil
}
