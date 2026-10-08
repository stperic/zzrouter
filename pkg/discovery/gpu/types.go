// Package gpu is the single source of truth for GPU detection
// across zzRouter. It answers three questions in a single pass:
//
//  1. Is each vendor's driver stack usable on this node? —
//     tri-state Probe / ProbeContext returning Detection with
//     StateDriverOK / StateHardwareNoDriver / StateAbsent so
//     install-time policy can gate on it.
//
//  2. What GPU cards are physically present, in PCI bus order? —
//     Inventory / InventoryContext returning a flat Card slice
//     with PCI address, product name, VRAM, driver version, and
//     compute capability joined by bus id across nvidia-smi,
//     rocm-smi, and ghw.
//
//  3. What do the two views look like together? — an Inventory
//     carries both the per-vendor Detection map and the per-card
//     slice, so one invocation feeds every consumer in the tree
//     without duplicating subprocess calls.
//
// Probes must never return an error for "no GPU" — that's a valid
// answer (StateAbsent). Errors are reserved for unexpected
// failures (shell-out crashed, library load failed unexpectedly).
//
// Callers:
//
//   - pkg/prov_apps/install — tri-state gate for Ollama/llama.cpp
//     (advisory) and vLLM (required). Consumes Detection.
//   - pkg/discovery/hardware — runtime discovery endpoint.
//     Consumes Inventory via a thin adapter that reshapes Cards
//     into the legacy HardwareInfo / GPUInfo contract.
//   - pkg/cluster/resource_tracker — live runtime metrics
//     (memory.used, utilization.gpu) via LiveMetricsContext.
//
// Every subprocess primitive (nvidia-smi / rocm-smi shell-out,
// field constants, row parsing) is package-internal by design:
// callers outside this package consume the typed results, not
// the raw query helpers. That invariant is load-bearing for
// the tri-state / inventory / live-metrics semantics the three
// entry points enforce.
package gpu

import (
	"fmt"
	"strings"
)

// Vendor identifies a GPU vendor whose drivers we probe for. The
// values match the lowercase strings used in provider config variant
// filters and HardwareInfo.GPUType so upstream code can switch on
// them without conversion helpers.
type Vendor string

const (
	VendorNVIDIA Vendor = "nvidia"
	VendorAMD    Vendor = "amd"
	VendorApple  Vendor = "apple"
)

// PCI vendor IDs used by the hardware probes. Kept here so every
// platform implementation references the same constants.
const (
	PCIVendorNVIDIA = "10de"
	PCIVendorAMD    = "1002"
)

// State is the tri-state result of a probe.
type State int

const (
	// StateUnknown is the zero value — only returned when the probe
	// itself failed in an unexpected way (e.g., platform not
	// supported). Callers should treat it the same as StateAbsent
	// for gating purposes and surface the error to logs.
	StateUnknown State = iota

	// StateAbsent means no matching GPU hardware was found on the
	// PCI/SetupAPI scan. The driver probe was not attempted.
	StateAbsent

	// StateHardwareNoDriver means hardware is present but the user-
	// space driver stack is missing or broken. The detection carries
	// an InstallHint for user guidance.
	StateHardwareNoDriver

	// StateDriverOK means hardware is present and the driver stack
	// loaded successfully. Name / DriverVersion / ComputeCapability
	// are populated when the probe could extract them.
	StateDriverOK
)

// String returns a short diagnostic form used in preflight messages.
func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateHardwareNoDriver:
		return "hardware-present/no-driver"
	case StateDriverOK:
		return "driver-ok"
	default:
		return "unknown"
	}
}

// Detection is the full result of a probe. All fields are populated
// on a best-effort basis — zero values mean "not known", not "zero".
type Detection struct {
	Vendor            Vendor `json:"vendor"`
	State             State  `json:"state"`
	Name              string `json:"name,omitempty"`               // product name ("NVIDIA RTX 4090")
	DriverVersion     string `json:"driver_version,omitempty"`     // "550.54.14"
	ComputeCapability string `json:"compute_capability,omitempty"` // "89" for 8.9
	MemoryMB          int64  `json:"memory_mb,omitempty"`
	Count             int    `json:"count,omitempty"`        // number of matching devices; 0 when the state isn't DriverOK. Useful for consumers aggregating by vendor.
	InstallHint       string `json:"install_hint,omitempty"` // populated when State == StateHardwareNoDriver
	Diagnostic        string `json:"diagnostic,omitempty"`   // free-form text from a failed probe — typically the stderr line from nvidia-smi / rocm-smi when the kernel module is missing or mismatched. Empty on the happy path.
}

// Policy controls how a caller wants the probe result turned into a
// pass/fail decision. Install preflight uses this to gate vLLM
// (PolicyRequired) vs. Ollama/llama.cpp (PolicyAdvisory).
type Policy string

const (
	// PolicyNone means the caller doesn't care — the probe is not
	// invoked at all. Provided as an explicit zero value so config
	// files can omit the field.
	PolicyNone Policy = ""

	// PolicyAdvisory means the caller wants an informational report:
	// the install proceeds in every state, but StateHardwareNoDriver
	// and StateAbsent surface a warning with the install hint.
	PolicyAdvisory Policy = "advisory"

	// PolicyRequired means the caller wants a hard gate: only
	// StateDriverOK is acceptable; every other state is a failure.
	PolicyRequired Policy = "required"
)

// ParseVendor canonicalizes a user-supplied vendor string and
// returns an error on unknown values. Use this at config-load
// time so typos like "nvdia" fail loudly instead of silently
// returning StateAbsent from the probe.
//
// An empty input returns ("", nil) — the caller is responsible
// for supplying a default in that case.
func ParseVendor(s string) (Vendor, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case string(VendorNVIDIA):
		return VendorNVIDIA, nil
	case string(VendorAMD):
		return VendorAMD, nil
	case string(VendorApple):
		return VendorApple, nil
	default:
		return "", fmt.Errorf("gpu: unknown vendor %q (want nvidia, amd, or apple)", s)
	}
}

// ParsePolicy canonicalizes a user-supplied policy string and
// returns an error on unknown values. An empty input returns
// (PolicyNone, nil).
func ParsePolicy(s string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return PolicyNone, nil
	case string(PolicyAdvisory):
		return PolicyAdvisory, nil
	case string(PolicyRequired):
		return PolicyRequired, nil
	default:
		return "", fmt.Errorf("gpu: unknown policy %q (want advisory or required)", s)
	}
}
