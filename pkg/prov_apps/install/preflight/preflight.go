// Package preflight runs pre-install checks — Python/venv presence,
// required CLIs, GPU driver state, disk space, write permissions —
// and reports them in a uniform Result/Report shape. Each check is
// independent; composition happens in the per-provider installer.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// ErrNoWritePermission is raised by WritePermission when the probe
// file cannot be created. Root re-exports this sentinel so existing
// errors.Is matches keep working for callers that imported install
// before the preflight split.
var ErrNoWritePermission = errors.New("no write permission")

// Result captures the outcome of a preflight check.
type Result struct {
	Check   string `json:"check"` // What was checked (e.g., "python3", "nvidia-gpu")
	Passed  bool   `json:"passed"`
	Message string `json:"message"`        // Human-readable status or error
	Hint    string `json:"hint,omitempty"` // Actionable fix suggestion (only for failures)
}

// Report is the result of all preflight checks for a provider.
type Report struct {
	Provider string   `json:"provider"`
	Results  []Result `json:"results"`
	AllOK    bool     `json:"all_ok"`
}

// VenvModule checks that the given Python interpreter has the `venv`
// module wired up (on Debian/Ubuntu, `python3-venv` ships separately
// and gets installed independently from the interpreter itself). It
// runs `<python> -m venv --help` rather than probing pip because venv
// is what the installer actually uses to bootstrap the provider env.
// An empty python path returns a single failing Result — callers must
// resolve an interpreter via the `interpreter` package before calling.
func VenvModule(ctx context.Context, python string) Result {
	if python == "" {
		return Result{
			Check:   "python venv",
			Passed:  false,
			Message: "no python interpreter resolved (select one first)",
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := host.CommandContext(probeCtx, python, "-m", "venv", "--help").Run(); err != nil {
		hint := "Install the venv module for your Python installation"
		if _, debErr := exec.LookPath("apt"); debErr == nil {
			hint = "Run: sudo apt install python3-venv (matching the interpreter's minor)"
		} else if _, brewErr := exec.LookPath("brew"); brewErr == nil {
			hint = "Homebrew Python includes venv by default. Try: brew reinstall python@3.13 (or matching minor)"
		}
		return Result{
			Check:   "python venv",
			Passed:  false,
			Message: fmt.Sprintf("venv module not available on %s", python),
			Hint:    hint,
		}
	}
	return Result{
		Check:   "python venv",
		Passed:  true,
		Message: fmt.Sprintf("venv module available on %s", python),
	}
}

// Commands verifies that a list of system commands are available in PATH.
func Commands(commands ...string) []Result {
	var results []Result
	for _, cmd := range commands {
		if path, err := exec.LookPath(cmd); err != nil {
			results = append(results, Result{
				Check:   cmd,
				Passed:  false,
				Message: fmt.Sprintf("%s not found in PATH", cmd),
				Hint:    commandInstallHint(cmd),
			})
		} else {
			results = append(results, Result{
				Check:   cmd,
				Passed:  true,
				Message: fmt.Sprintf("found at %s", path),
			})
		}
	}
	return results
}

// commandInstallHint returns a platform-aware install suggestion for a command.
func commandInstallHint(cmd string) string {
	hasBrew := false
	if _, err := exec.LookPath("brew"); err == nil {
		hasBrew = true
	}
	hasApt := false
	if _, err := exec.LookPath("apt"); err == nil {
		hasApt = true
	}

	switch cmd {
	case "curl":
		if hasApt {
			return "Run: sudo apt install curl"
		}
		if hasBrew {
			return "Run: brew install curl"
		}
		return "Install curl: https://curl.se/download.html"
	case "tar":
		if hasApt {
			return "Run: sudo apt install tar"
		}
		return "Install tar using your system package manager"
	case "powershell":
		return "PowerShell should be pre-installed on Windows. Check your PATH."
	default:
		if hasApt {
			return fmt.Sprintf("Try: sudo apt install %s", cmd)
		}
		if hasBrew {
			return fmt.Sprintf("Try: brew install %s", cmd)
		}
		return fmt.Sprintf("Install %s using your system package manager", cmd)
	}
}

// GPU probes the requested vendor via pkg/discovery/gpu and
// converts the tri-state result into a Result according to
// the provider's declared policy.
//
//   - Policy "required" (vLLM): only StateDriverOK passes. Both
//     StateAbsent and StateHardwareNoDriver mark the check failed;
//     the hint from the probe propagates so the user knows whether
//     to install a driver or give up on the node entirely.
//
//   - Policy "advisory" (Ollama, llama.cpp CUDA variant): every
//     probe state passes — install always proceeds. StateDriverOK
//     records a positive message; the other states record an
//     informational warning with the install hint so the operator
//     sees it in the preflight report but isn't blocked.
//
// A nil or empty GPURequirement short-circuits with no check,
// matching AppRequirements semantics elsewhere: "unset" means "skip".
// Vendor and Policy strings are parsed through gpu.ParseVendor and
// gpu.ParsePolicy, so typos fail loudly at preflight time rather
// than silently returning StateAbsent.
func GPU(ctx context.Context, req *config.GPURequirement) (Result, bool) {
	if req == nil {
		return Result{}, false
	}

	vendor, err := gpu.ParseVendor(req.Vendor)
	if err != nil {
		return Result{
			Check:   "gpu",
			Passed:  false,
			Message: err.Error(),
			Hint:    "Set requirements.gpu.vendor to nvidia, amd, or apple in provider config",
		}, true
	}
	if vendor == "" {
		vendor = gpu.VendorNVIDIA
	}

	policy, err := gpu.ParsePolicy(req.Policy)
	if err != nil {
		return Result{
			Check:   gpuCheckName(vendor),
			Passed:  false,
			Message: err.Error(),
			Hint:    "Set requirements.gpu.policy to advisory or required in provider config",
		}, true
	}
	if policy == gpu.PolicyNone {
		policy = gpu.PolicyRequired
	}

	// 10s is an upper bound; WithTimeout inherits any shorter
	// deadline already on ctx. When ctx carries a cached inventory
	// the probe is a map lookup and the timeout is never exercised.
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	det, err := gpu.ProbeContext(probeCtx, vendor)
	if err != nil {
		// Probe itself is unsupported on this platform — treat as
		// "no GPU detected" rather than a preflight error, matching
		// how Ollama's installer silently falls through to CPU.
		return Result{
			Check:   gpuCheckName(vendor),
			Passed:  policy != gpu.PolicyRequired,
			Message: fmt.Sprintf("gpu probe unsupported: %v", err),
		}, true
	}

	return gpuDetectionToResult(det, policy), true
}

// gpuDetectionToResult applies the provider's policy to the tri-
// state probe result. The "passed" bit turns the report into a hard
// gate for PolicyRequired and a soft warning for PolicyAdvisory.
//
// StateUnknown is treated as a hard failure under both policies —
// the probe returned something the policy machine doesn't understand,
// which means bigger problems upstream and we'd rather surface it
// than let the install proceed blindly.
func gpuDetectionToResult(det gpu.Detection, policy gpu.Policy) Result {
	name := gpuCheckName(det.Vendor)
	switch det.State {
	case gpu.StateDriverOK:
		return Result{
			Check:   name,
			Passed:  true,
			Message: gpuOKMessage(det),
		}
	case gpu.StateHardwareNoDriver:
		return Result{
			Check:   name,
			Passed:  policy == gpu.PolicyAdvisory,
			Message: fmt.Sprintf("%s hardware detected but driver is not loaded", strings.ToUpper(string(det.Vendor))),
			Hint:    det.InstallHint,
		}
	case gpu.StateAbsent:
		return Result{
			Check:   name,
			Passed:  policy == gpu.PolicyAdvisory,
			Message: fmt.Sprintf("no %s GPU detected on this node", strings.ToUpper(string(det.Vendor))),
			Hint:    det.InstallHint,
		}
	case gpu.StateUnknown:
		return Result{
			Check:   name,
			Passed:  false,
			Message: "gpu probe returned unknown state: this should never happen; report a bug",
		}
	default:
		// Any future State value that preflight hasn't learned
		// about yet. Fail loudly so the gap is obvious.
		return Result{
			Check:   name,
			Passed:  false,
			Message: fmt.Sprintf("gpu probe returned unexpected state %d", det.State),
		}
	}
}

func gpuCheckName(vendor gpu.Vendor) string {
	if vendor == "" {
		return "gpu"
	}
	return string(vendor) + "-gpu"
}

func gpuOKMessage(det gpu.Detection) string {
	parts := make([]string, 0, 3)
	if det.Name != "" {
		parts = append(parts, det.Name)
	}
	if det.DriverVersion != "" {
		parts = append(parts, "driver "+det.DriverVersion)
	}
	if det.ComputeCapability != "" {
		parts = append(parts, "cc "+det.ComputeCapability)
	}
	if len(parts) == 0 {
		return strings.ToUpper(string(det.Vendor)) + " driver OK"
	}
	return strings.Join(parts, ", ")
}

// DiskAndPermissions verifies disk space and write permissions for the install directory.
// minDiskSpace is a human-readable size from config (e.g., "500MB", "10GB"). Empty = skip.
func DiskAndPermissions(providerName, minDiskSpace string) []Result {
	var results []Result
	installDir := fsroot.ProviderDir(providerName)

	// Write permission
	if err := WritePermission(installDir); err != nil {
		results = append(results, Result{
			Check:   "write-permission",
			Passed:  false,
			Message: fmt.Sprintf("No write permission on %s", installDir),
			Hint:    fmt.Sprintf("Check directory ownership and permissions: ls -la %s", installDir),
		})
	} else {
		results = append(results, Result{
			Check:   "write-permission",
			Passed:  true,
			Message: fmt.Sprintf("writable: %s", installDir),
		})
	}

	// Disk space
	if required := ParseSize(minDiskSpace); required > 0 {
		if err := DiskSpace(installDir, required); err != nil {
			results = append(results, Result{
				Check:   "disk-space",
				Passed:  false,
				Message: err.Error(),
				Hint:    fmt.Sprintf("Free up at least %s of disk space in %s", minDiskSpace, installDir),
			})
		} else {
			results = append(results, Result{
				Check:   "disk-space",
				Passed:  true,
				Message: fmt.Sprintf(">= %s available", minDiskSpace),
			})
		}
	}

	return results
}

// ParseSize parses a human-readable size string (e.g., "500MB", "10GB") to bytes.
// Supports KB, MB, GB, TB suffixes (case-insensitive). Returns 0 for empty/unparseable strings.
func ParseSize(s string) uint64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}

	multipliers := map[string]uint64{
		"KB": 1024,
		"MB": 1024 * 1024,
		"GB": 1024 * 1024 * 1024,
		"TB": 1024 * 1024 * 1024 * 1024,
	}

	for suffix, mult := range multipliers {
		if before, ok := strings.CutSuffix(s, suffix); ok {
			numStr := strings.TrimSpace(before)
			if val, err := strconv.ParseFloat(numStr, 64); err == nil {
				return uint64(val * float64(mult))
			}
		}
	}

	// Try plain number (bytes)
	if val, err := strconv.ParseUint(s, 10, 64); err == nil {
		return val
	}
	return 0
}
