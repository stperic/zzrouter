package schema

import (
	"bytes"
	"fmt"

	"regexp"

	"gopkg.in/yaml.v3"
)

// Diagnostics declares release-owned checks, never executable recipe text.
type Diagnostics struct {
	Runtime  string                   `yaml:"runtime" json:"runtime"`
	Runtimes map[string]RuntimeChecks `yaml:"runtimes" json:"runtimes"`
	Memory   *MemoryBudget            `yaml:"memory,omitempty" json:"memory,omitempty"`
}

// RuntimeChecks describes fixed probes applied to an installed runtime.
type RuntimeChecks struct {
	Imports        []string          `yaml:"imports,omitempty" json:"imports,omitempty"`
	Packages       []string          `yaml:"packages,omitempty" json:"packages,omitempty"`
	Checks         []string          `yaml:"checks,omitempty" json:"checks,omitempty"`
	Kernels        string            `yaml:"kernels" json:"kernels"`
	ToolkitMinimum map[string]string `yaml:"toolkit_minimum,omitempty" json:"toolkit_minimum,omitempty"`
	Workaround     map[string]string `yaml:"workaround,omitempty" json:"workaround,omitempty"`
}

// MemoryBudget declares how an engine's knob requests device memory.
type MemoryBudget struct {
	Kind                 string `yaml:"kind" json:"kind"`
	Parameter            string `yaml:"parameter,omitempty" json:"parameter,omitempty"`
	DeviceCountParameter string `yaml:"device_count_parameter,omitempty" json:"device_count_parameter,omitempty"`
	AutoSafetyMarginMiB  int64  `yaml:"auto_safety_margin_mib,omitempty" json:"auto_safety_margin_mib,omitempty"`
}

// UnmarshalYAML rejects unknown diagnostic fields without changing older schemas.
func (d *Diagnostics) UnmarshalYAML(node *yaml.Node) error {
	type plain Diagnostics
	data, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("encode diagnostics: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var out plain
	if err := decoder.Decode(&out); err != nil {
		return fmt.Errorf("decode diagnostics: %w", err)
	}
	*d = Diagnostics(out)
	return nil
}

// Validate rejects executable text and unknown fixed probe kinds.
func (d *Diagnostics) Validate() error {
	if d == nil {
		return nil
	}
	if len(d.Runtimes) == 0 || len(d.Runtimes) > 16 {
		return fmt.Errorf("diagnostics requires 1 to 16 runtimes")
	}
	if _, ok := d.Runtimes[d.Runtime]; !ok {
		return fmt.Errorf("diagnostics runtime %q not declared", d.Runtime)
	}
	for name, checks := range d.Runtimes {
		if !validDiagnosticName(name) {
			return fmt.Errorf("invalid diagnostics runtime %q", name)
		}
		if err := checks.validate(); err != nil {
			return fmt.Errorf("diagnostics runtime %q: %w", name, err)
		}
	}
	return d.Memory.validate()
}

func validDiagnosticName(name string) bool {
	return len(name) <= 128 && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(name)
}

func (r RuntimeChecks) validate() error {
	if len(r.Imports) > 32 || len(r.Packages) > 32 || len(r.Checks) > 8 || len(r.ToolkitMinimum) > 16 || len(r.Workaround) > 16 {
		return fmt.Errorf("probe limits exceeded")
	}
	identifier := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	for _, module := range r.Imports {
		if len(module) > 128 || !identifier.MatchString(module) {
			return fmt.Errorf("invalid diagnostic import %q", module)
		}
	}
	for _, pkg := range r.Packages {
		if !validDiagnosticName(pkg) {
			return fmt.Errorf("invalid diagnostic package %q", pkg)
		}
	}
	if err := r.validateChecks(); err != nil {
		return err
	}
	switch r.Kernels {
	case "prebuilt", "jit", "prebuilt_or_jit", "external", "unknown":
	default:
		return fmt.Errorf("unknown kernel mode %q", r.Kernels)
	}
	version := regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	capability := regexp.MustCompile(`^[0-9]{1,2}$`)
	for arch, minimum := range r.ToolkitMinimum {
		if !capability.MatchString(arch) || len(minimum) > 8 || !version.MatchString(minimum) {
			return fmt.Errorf("invalid toolkit requirement %q: %q", arch, minimum)
		}
	}
	environment := regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	for key, value := range r.Workaround {
		if len(key) > 128 || !environment.MatchString(key) || (value != "0" && value != "1") {
			return fmt.Errorf("invalid diagnostic workaround %q", key)
		}
	}
	return nil
}

func (r RuntimeChecks) validateChecks() error {
	seen := make(map[string]bool, len(r.Checks))
	for _, check := range r.Checks {
		switch check {
		case "pip_check", "imports", "cuda_companions", "cuda_available", "metal_available", "managed_toolkit":
		default:
			return fmt.Errorf("unknown diagnostic check %q", check)
		}
		if seen[check] {
			return fmt.Errorf("duplicate diagnostic check %q", check)
		}
		seen[check] = true
	}
	if seen["imports"] && len(r.Imports) == 0 {
		return fmt.Errorf("imports check requires modules")
	}
	if len(r.Imports) > 0 && (!seen["imports"] || !seen["pip_check"]) {
		return fmt.Errorf("python diagnostics require imports and pip_check")
	}
	return nil
}

func (m *MemoryBudget) validate() error {
	if m == nil {
		return nil
	}
	if m.AutoSafetyMarginMiB < 0 {
		return fmt.Errorf("auto safety margin must not be negative")
	}
	for _, parameter := range []string{m.Parameter, m.DeviceCountParameter} {
		if parameter != "" && !validDiagnosticName(parameter) {
			return fmt.Errorf("invalid memory parameter %q", parameter)
		}
	}
	switch m.Kind {
	case "fraction_total":
		if m.Parameter == "" {
			return fmt.Errorf("fraction budget requires parameter")
		}
	case "layers", "unified", "external":
	default:
		return fmt.Errorf("unknown memory budget %q", m.Kind)
	}
	return nil
}
