//go:build linux

package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/utils"
)

// lspciRowPrefix anchors hasPCIVendorMatch to real device rows.
// lspci prints `BB:DD.F class: vendor name` (or the 4-hex-domain
// form on hosts with `-D`) as the very first token; diagnostic
// warnings from busybox-pciutils and friends start with prose
// like "Cannot open /proc/bus/pci", which must not be classified
// as a match.
var lspciRowPrefix = regexp.MustCompile(`(?i)^([0-9a-f]{4}:)?[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]`)

// linuxProbeOptions threads the small set of injection seams the
// Linux probe needs through one struct, so neither tests nor
// production code reaches a package-level mutable global. The
// public Probe / ProbeContext API is unaffected — it always uses
// defaultLinuxProbeOptions().
//
// Every field has a defaultable form so a partial struct works:
//   - sysfsRoot defaults to "/sys/bus/pci/devices"
//   - kfdPath defaults to "/sys/class/kfd/kfd"
//   - drmGlob defaults to "/sys/class/drm/card*/device/vendor"
//   - queryNVIDIASMI defaults to queryNVIDIASMI
type linuxProbeOptions struct {
	sysfsRoot      string
	kfdPath        string
	drmGlob        string
	queryNVIDIASMI func(ctx context.Context, fields ...string) ([]nvidiaRow, error)
}

// withDefaults fills in zero-valued fields with the production
// defaults. Tests can pass a partial struct and only override the
// pieces they care about.
func (o linuxProbeOptions) withDefaults() linuxProbeOptions {
	if o.sysfsRoot == "" {
		o.sysfsRoot = "/sys/bus/pci/devices"
	}
	if o.kfdPath == "" {
		o.kfdPath = "/sys/class/kfd/kfd"
	}
	if o.drmGlob == "" {
		// Anchor on the digit suffix so the glob doesn't catch
		// renderD* nodes (which share the same PCI parent as
		// their sibling card* and would dedupe to the same
		// canonical path anyway, but skipping them avoids a
		// pointless stat per render minor).
		o.drmGlob = "/sys/class/drm/card[0-9]*/device/vendor"
	}
	if o.queryNVIDIASMI == nil {
		o.queryNVIDIASMI = queryNVIDIASMI
	}
	return o
}

// probePlatform is the Linux entry point. It follows Ollama's
// install-time approach: cheap PCI scan first to determine hardware
// presence, then a driver probe via nvidia-smi or sysfs. The scan
// uses lspci when available and falls back to walking
// /sys/bus/pci/devices so minimal containers without pciutils
// still work.
func probePlatform(ctx context.Context, vendor Vendor) (Detection, error) {
	return probePlatformLinux(ctx, vendor, linuxProbeOptions{}.withDefaults())
}

// probePlatformLinux is the test-injection seam — it accepts the
// options struct so unit tests can point the sysfs walker at a
// tempdir, stub out queryNVIDIASMI to fake nvidia-smi presence
// without touching PATH, etc. Production callers go through
// probePlatform with the defaults.
func probePlatformLinux(ctx context.Context, vendor Vendor, opts linuxProbeOptions) (Detection, error) {
	switch vendor {
	case VendorNVIDIA:
		return probeNVIDIALinux(ctx, opts)
	case VendorAMD:
		return probeAMDLinux(ctx, opts)
	case VendorApple:
		// Apple Silicon never appears on Linux.
		return Detection{Vendor: vendor, State: StateAbsent}, nil
	default:
		return Detection{Vendor: vendor, State: StateUnknown},
			fmt.Errorf("gpu: unsupported vendor %q", vendor)
	}
}

func probeNVIDIALinux(ctx context.Context, opts linuxProbeOptions) (Detection, error) {
	d := Detection{Vendor: VendorNVIDIA, State: StateAbsent}

	hasHardware := linuxHasPCIVendor(ctx, PCIVendorNVIDIA, opts.sysfsRoot)
	// Cancellation must not be silently reclassified — the caller
	// pulled the plug, the driver state is unknown.
	if cerr := ctx.Err(); cerr != nil {
		return d, cerr
	}

	// nvidia-smi is the canonical driver-loaded signal: it ships
	// with every NVIDIA driver package and fails fast when the
	// kernel module isn't loaded. We attempt it even when the PCI
	// scan came up empty so WSL2 — which has an empty
	// /sys/bus/pci/devices but ships nvidia-smi via
	// /usr/lib/wsl/lib — is still reported as StateDriverOK.
	rows, err := opts.queryNVIDIASMI(ctx,
		nvidiaFieldName,
		nvidiaFieldDriverVersion,
		nvidiaFieldMemoryTotalMiB,
		nvidiaFieldComputeCap,
	)
	if cerr := ctx.Err(); cerr != nil {
		return d, cerr
	}

	switch {
	case len(rows) > 0:
		d.State = StateDriverOK
		d.Count = len(rows)
		populateNVIDIADetection(&d, rows[0])
		return d, nil
	case err != nil:
		// nvidia-smi exists but errored — driver almost certainly
		// in a broken state. The wrapped error from queryNVIDIASMI
		// already includes nvidia-smi's stderr; surface it via
		// Diagnostic so operators see the real story (NVML version
		// mismatch, kernel module missing, etc.).
		d.State = StateHardwareNoDriver
		d.Diagnostic = err.Error()
		d.InstallHint = InstallHint(VendorNVIDIA)
		return d, nil //nolint:nilerr // A broken driver is a successful detection; its error is reported in Diagnostic.
	case hasHardware:
		// Hardware on the bus but nvidia-smi isn't installed at
		// all — classic "user installed the GPU but never installed
		// the proprietary driver" state.
		d.State = StateHardwareNoDriver
		d.InstallHint = InstallHint(VendorNVIDIA)
		return d, nil
	default:
		// No hardware, no driver — neither WSL2 nor a managed
		// host. StateAbsent stands.
		return d, nil
	}
}

func probeAMDLinux(ctx context.Context, opts linuxProbeOptions) (Detection, error) {
	d := Detection{Vendor: VendorAMD, State: StateAbsent}

	if !linuxHasPCIVendor(ctx, PCIVendorAMD, opts.sysfsRoot) {
		if err := ctx.Err(); err != nil {
			return d, err
		}
		return d, nil
	}
	if err := ctx.Err(); err != nil {
		return d, err
	}
	d.State = StateHardwareNoDriver

	// /sys/class/kfd/kfd is exposed by the amdkfd kernel module that
	// the ROCm runtime needs; its presence is Ollama's own criterion
	// for "AMD compute stack loaded". The render-node check catches
	// the amdgpu-only case (ROCm installed without KFD on older chips
	// where compute is disabled).
	if _, err := os.Stat(opts.kfdPath); err == nil {
		d.State = StateDriverOK
	} else if amdRenderNodeCount(opts.drmGlob) > 0 {
		d.State = StateDriverOK
	}

	if d.State == StateDriverOK {
		// Best-effort: count AMD cards by walking /sys/class/drm.
		// At least 1 when the AMD driver is loaded but no cards
		// are enumerated (which shouldn't happen on a real system
		// but keeps Count consistent with State == DriverOK).
		n := amdRenderNodeCount(opts.drmGlob)
		if n == 0 {
			n = 1
		}
		d.Count = n
	}

	if d.State == StateHardwareNoDriver {
		d.InstallHint = InstallHint(VendorAMD)
	}
	return d, nil
}

// amdRenderNodeCount walks the supplied DRM card-vendor glob and
// returns the number of distinct AMD PCI devices reporting the
// AMD vendor id. Distinct means we dedupe by the canonical path
// of card*/device: on modern kernels the same physical GPU can
// be exposed as both card0 and card1 (primary + render minors
// on boards with multiple display pipes), and naive counting
// over-reports Detection.Count by a factor of 2. EvalSymlinks
// on the card directory collapses those minors down to the
// single /sys/devices/pci.../.../XXXX:XX:XX.X node.
//
// Used both as a "AMD driver loaded" signal (count > 0) and as
// the source for Detection.Count on AMD systems.
func amdRenderNodeCount(glob string) int {
	entries, err := filepath.Glob(glob)
	if err != nil {
		return 0
	}
	want := "0x" + strings.ToLower(PCIVendorAMD)
	seen := make(map[string]struct{}, len(entries))
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(string(data)), want) {
			continue
		}
		// Dedupe by the canonical device path. filepath.Dir(path)
		// is card*/device; EvalSymlinks resolves that to the
		// underlying PCI node. If resolution fails (dangling
		// symlink under a test fixture, rare on real sysfs), fall
		// back to the unresolved path so the card is still counted.
		deviceDir := filepath.Dir(path)
		key, lerr := filepath.EvalSymlinks(deviceDir)
		if lerr != nil || key == "" {
			key = deviceDir
		}
		seen[key] = struct{}{}
	}
	return len(seen)
}

// linuxHasPCIVendor returns true when a PCI device matching vendorID
// (hex, lowercase, no "0x") is present. Uses lspci when available
// and falls back to walking the supplied sysfs root so minimal
// containers without pciutils still work.
//
// lspci is invoked with .Output() so its stderr stays segregated:
// folding stderr into stdout via CombinedOutput would let a benign
// pcilib warning ("Cannot open /proc/bus/pci") get classified as a
// match by the row-counting check, upgrading a no-GPU host to
// StateHardwareNoDriver. On the error path we pull stderr from
// *exec.ExitError so the operator-visible warning still includes
// what lspci complained about.
func linuxHasPCIVendor(ctx context.Context, vendorID, sysfsRoot string) bool {
	out, err := host.CommandContext(ctx, "lspci",
		"-d", vendorID+":").Output()
	switch {
	case err == nil:
		if hasPCIVendorMatch(out, vendorID) {
			return true
		}
	default:
		// lspci absent / broken — log only when we actually have
		// stderr to surface, so missing-binary stays silent.
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			utils.LogWarnf("lspci returned %v: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
	}

	return sysfsHasPCIVendor(sysfsRoot, vendorID)
}

// sysfsHasPCIVendor walks a /sys/bus/pci/devices-shaped tree
// looking for a child vendor file whose contents are "0xNNNN"
// matching vendorID. Pure — takes the tree root as a parameter
// so tests can inject a tempdir fixture.
func sysfsHasPCIVendor(root, vendorID string) bool {
	entries, err := filepath.Glob(filepath.Join(root, "*", "vendor"))
	if err != nil {
		return false
	}
	want := "0x" + strings.ToLower(vendorID)
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(data)), want) {
			return true
		}
	}
	return false
}

// hasPCIVendorMatch checks whether lspci's -d output actually
// contains a device row for the requested vendor. lspci with a
// matching device prints lines like
//
//	01:00.0 VGA compatible controller: NVIDIA Corporation ...
//
// and exits 0 even when the match set is empty, so testing
// stdout for the vendor hex is the correct check.
//
// We require each candidate line to start with a BDF token so
// busybox-pciutils forks that emit diagnostics on stdout (e.g.
// "Cannot open /proc/bus/pci") don't get classified as a match
// and flip a no-GPU host into StateHardwareNoDriver. vendorID
// is accepted for symmetry with the sysfs walker and to keep
// the signature stable; lspci's own `-d vendor:` filter is
// trusted for the vendor check.
func hasPCIVendorMatch(out []byte, vendorID string) bool {
	_ = vendorID
	if len(out) == 0 {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if lspciRowPrefix.MatchString(strings.TrimLeft(line, " \t")) {
			return true
		}
	}
	return false
}
