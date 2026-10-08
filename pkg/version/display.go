package version

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// PrintMode selects the output shape for Print.
type PrintMode int

const (
	PrintSimple PrintMode = iota
	PrintDetailed
	PrintJSON
)

// PrintSpec is the per-binary configuration for the CLI `version` command.
// WithCompatibility adds a Cluster/Client compatibility footer on detailed
// output and switches JSON output to the with-compatibility envelope; the
// client binary sets it, the node does not.
type PrintSpec struct {
	BinaryName        string
	ServiceType       string
	Capabilities      []string
	WithCompatibility bool
}

// Print renders version information for spec into w using mode. It is the
// single entry point shared by both CLI binaries' `version` subcommand.
func Print(w io.Writer, spec PrintSpec, mode PrintMode) error {
	switch mode {
	case PrintSimple:
		_, err := fmt.Fprint(w, FormatVersionSimple(spec.BinaryName))
		return err
	case PrintDetailed:
		if _, err := fmt.Fprint(w, FormatVersionDetailed(spec.BinaryName, spec.ServiceType, spec.Capabilities)); err != nil {
			return err
		}
		if spec.WithCompatibility {
			fmt.Fprintf(w, "\nCompatibility:\n")
			fmt.Fprintf(w, "  Cluster protocol: %s\n",
				formatProtocolWindow(ClusterProtocolVersion, MinClusterProtocolVersion))
			fmt.Fprintf(w, "  Cluster nodes (display floor): >= %s\n", ClusterCompatibility.MinSupportedVersion.String())
			fmt.Fprintf(w, "  Admin API: >= %s\n", ClientCompatibility.MinSupportedVersion.String())
		}
		return nil
	case PrintJSON:
		var (
			out string
			err error
		)
		if spec.WithCompatibility {
			out, err = FormatVersionJSONWithCompatibility(spec.ServiceType, spec.Capabilities)
		} else {
			out, err = FormatVersionJSON(spec.ServiceType, spec.Capabilities)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, out)
		return err
	default:
		return fmt.Errorf("version: unknown print mode %d", mode)
	}
}

// FormatVersionSimple returns a simple version string with git commit and build date
func FormatVersionSimple(binaryName string) string {
	v := Current
	result := fmt.Sprintf("%s version %s\n", binaryName, v.String())

	if GitCommit != "unknown" {
		result += fmt.Sprintf("Git commit: %s\n", GitCommit)
	}

	if BuildDate != "unknown" {
		result += fmt.Sprintf("Built: %s\n", BuildDate)
	}

	return result
}

// FormatVersionDetailed returns detailed version information
func FormatVersionDetailed(binaryName, serviceType string, capabilities []string) string {
	versionInfo := GetCurrentVersionInfo(serviceType, capabilities)

	var result strings.Builder
	result.WriteString(fmt.Sprintf("%s Version Information\n", binaryName))
	result.WriteString("================================\n\n")

	// Version details
	result.WriteString(fmt.Sprintf("Version: %s\n", versionInfo.Version.String()))
	result.WriteString(fmt.Sprintf("  Major: %d\n", versionInfo.Version.Major))
	result.WriteString(fmt.Sprintf("  Minor: %d\n", versionInfo.Version.Minor))
	result.WriteString(fmt.Sprintf("  Patch: %d\n", versionInfo.Version.Patch))

	if versionInfo.Version.PreRelease != "" {
		result.WriteString(fmt.Sprintf("  Pre-release: %s\n", versionInfo.Version.PreRelease))
	}

	result.WriteString(fmt.Sprintf("\nAPI Version: %s\n", versionInfo.APIVersion))
	result.WriteString(fmt.Sprintf("Service Type: %s\n", versionInfo.ServiceType))
	result.WriteString(fmt.Sprintf("Cluster Protocol: %s\n",
		formatProtocolWindow(versionInfo.ClusterProtocol, versionInfo.MinClusterProtocol)))

	// Build information
	if versionInfo.BuildInfo != nil {
		result.WriteString("\nBuild Information:\n")
		result.WriteString(fmt.Sprintf("  Git Commit: %s\n", versionInfo.BuildInfo.GitCommit))
		result.WriteString(fmt.Sprintf("  Git Branch: %s\n", versionInfo.BuildInfo.GitBranch))
		result.WriteString(fmt.Sprintf("  Build Date: %s\n", versionInfo.BuildInfo.BuildDate.Format("2006-01-02 15:04:05 UTC")))
		result.WriteString(fmt.Sprintf("  Go Version: %s\n", versionInfo.BuildInfo.GoVersion))
		result.WriteString(fmt.Sprintf("  Platform: %s\n", versionInfo.BuildInfo.Platform))
		result.WriteString(fmt.Sprintf("  Build User: %s\n", versionInfo.BuildInfo.BuildUser))
	}

	// Capabilities
	if len(versionInfo.Capabilities) > 0 {
		result.WriteString("\nCapabilities:\n")
		for _, cap := range versionInfo.Capabilities {
			result.WriteString(fmt.Sprintf("  - %s\n", cap))
		}
	}

	return result.String()
}

// FormatVersionJSON returns version information as a JSON string
func FormatVersionJSON(serviceType string, capabilities []string) (string, error) {
	versionInfo := GetCurrentVersionInfo(serviceType, capabilities)

	jsonData, err := json.MarshalIndent(versionInfo, "", "  ")
	if err != nil {
		return "", fmt.Errorf("error marshaling version info: %w", err)
	}

	return string(jsonData), nil
}

// FormatVersionJSONWithCompatibility returns version information with compatibility info as JSON
func FormatVersionJSONWithCompatibility(serviceType string, capabilities []string) (string, error) {
	versionInfo := GetCurrentVersionInfo(serviceType, capabilities)

	output := map[string]any{
		"version_info": versionInfo,
		"compatibility": map[string]any{
			"cluster": map[string]any{
				// Authoritative gate: coordinator rejects peers outside this window.
				"protocol":     ClusterProtocolVersion,
				"min_protocol": MinClusterProtocolVersion,
				// Informational only (human-readable semver floor); does not gate.
				"min_supported_display": ClusterCompatibility.MinSupportedVersion.String(),
			},
			"client": map[string]any{
				"min_supported": ClientCompatibility.MinSupportedVersion.String(),
				"max_supported": func() string {
					if ClientCompatibility.MaxSupportedVersion != nil {
						return ClientCompatibility.MaxSupportedVersion.String()
					}
					return "unlimited"
				}(),
			},
		},
	}

	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", fmt.Errorf("error formatting JSON: %w", err)
	}

	return string(jsonBytes), nil
}

// formatProtocolWindow renders a protocol window for human display.
// Collapses the common Min==Max case to a single "vN" string; expands to
// "vN (accepts vMin..vMax)" when an N-1 compat window is active.
func formatProtocolWindow(proto, minProto int) string {
	if proto == minProto {
		return fmt.Sprintf("v%d", proto)
	}
	return fmt.Sprintf("v%d (accepts v%d..v%d)", proto, minProto, proto)
}

// GetServerVersionInfo returns version info for the server with standard capabilities
func GetServerVersionInfo() *VersionInfo {
	return GetCurrentVersionInfo(ServiceTypeHost, ServerCapabilities)
}
