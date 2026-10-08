package install

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// ManagedInterpreter locates the interpreter owned by a Python installer.
type ManagedInterpreter interface{ PythonPath() string }

// PackageVersion is read from installed distribution metadata without importing it.
type PackageVersion struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Reason    string `json:"reason"`
}

// CUDARuntime separates torch's build from the runtime library actually loaded.
type CUDARuntime struct {
	BuildVersion  string `json:"build_version"`
	LoadedVersion string `json:"loaded_version"`
	LibraryPath   string `json:"library_path"`
	Reason        string `json:"reason"`
}

// CUDAToolkit describes the compiler selected by CUDA discovery.
type CUDAToolkit struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Reason  string `json:"reason"`
}

// RuntimeDevice describes a device visible to this runtime.
type RuntimeDevice struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	Capability string `json:"capability"`
}

// RuntimeEnvironment contains fixed, read-only package and native-runtime observations.
type RuntimeEnvironment struct {
	Packages       map[string]PackageVersion `json:"packages"`
	CUDARuntime    CUDARuntime               `json:"cuda_runtime"`
	Toolkit        CUDAToolkit               `json:"toolkit"`
	CPPCompiler    CUDAToolkit               `json:"cpp_compiler"`
	Devices        []RuntimeDevice           `json:"devices"`
	MetalAvailable *bool                     `json:"metal_available"`
	MetalReason    string                    `json:"metal_reason"`
}

// InspectRuntime observes a managed interpreter under its effective launch environment.
func InspectRuntime(ctx context.Context, python string, checks schema.RuntimeChecks, environment map[string]string, toolkits ...ToolkitSelection) (*RuntimeEnvironment, error) {
	data, err := runtimeCheckData(checks)
	if err != nil {
		return nil, err
	}
	line, err := runRuntimeProbe(ctx, python, data, environment, "environment", toolkits...)
	if err != nil {
		return nil, err
	}
	var result RuntimeEnvironment
	if err := json.Unmarshal(line, &result); err != nil {
		return nil, fmt.Errorf("decode runtime environment: %w", err)
	}
	return &result, nil
}

// HostFinding explains a host prerequisite without authorizing a host change.
type HostFinding struct {
	Name             string            `json:"name"`
	Detected         string            `json:"detected"`
	Required         string            `json:"required"`
	Impact           string            `json:"impact"`
	HumanRemediation []string          `json:"human_remediation"`
	APIWorkarounds   map[string]string `json:"api_workarounds"`
}

// ToolkitFindings compares declared architecture requirements with the detected compiler.
func ToolkitFindings(checks schema.RuntimeChecks, toolkit CUDAToolkit, capabilities []string) []HostFinding {
	minimum := ""
	for _, capability := range capabilities {
		major := strings.SplitN(capability, ".", 2)[0]
		required := checks.ToolkitMinimum[major]
		if versionLess(minimum, required) {
			minimum = required
		}
	}
	if minimum == "" || !versionLess(toolkit.Version, minimum) {
		return nil
	}
	return []HostFinding{{
		Name: "cuda_toolkit", Detected: fmt.Sprintf("nvcc %s at %s", toolkit.Version, toolkit.Path), Required: "CUDA compiler >= " + minimum + " for the detected GPU architecture",
		Impact:         "Kernel paths using this toolkit can reject the GPU architecture or fail JIT compilation. Prebuilt kernels and configured API workarounds may bypass those paths.",
		APIWorkarounds: checks.Workaround,
		HumanRemediation: []string{
			"Have the host operator install or select a compatible CUDA toolkit meeting this requirement.",
			"Have the host operator make the provider service's CUDA_HOME, CUDA_PATH or PATH select that toolkit; zzRouter never installs a host toolkit.",
			"Repeat the environment and install/verify API calls after the operator's change.",
		},
	}}
}

// CompilerFindings explains the external compiler prerequisite for managed JIT.
func CompilerFindings(checks schema.RuntimeChecks, compiler CUDAToolkit) []HostFinding {
	if compiler.Path != "" {
		return nil
	}
	managed := false
	for _, check := range checks.Checks {
		managed = managed || check == "managed_toolkit"
	}
	if !managed {
		return nil
	}
	return []HostFinding{{Name: "cpp_compiler", Detected: compiler.Reason, Required: "A compatible human-provisioned C++ compiler on the service PATH", Impact: "The managed CUDA compiler cannot compile model-triggered kernels without its host C++ compiler.", HumanRemediation: []string{"Have the node operator provision the supported C++ compiler and make it visible to the service. zzRouter does not install host packages."}, APIWorkarounds: checks.Workaround}}
}

func versionLess(actual, required string) bool {
	var aMajor, aMinor, rMajor, rMinor int
	if _, err := fmt.Sscanf(required, "%d.%d", &rMajor, &rMinor); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(actual, "%d.%d", &aMajor, &aMinor); err != nil {
		return true
	}
	return aMajor < rMajor || (aMajor == rMajor && aMinor < rMinor)
}
