package version

import (
	"fmt"
	"strings"
)

// CompatibilityMatrix defines version compatibility rules
type CompatibilityMatrix struct {
	MinSupportedVersion  *Version         `json:"min_supported_version"`
	MaxSupportedVersion  *Version         `json:"max_supported_version,omitempty"` // Optional
	BreakingChanges      []BreakingChange `json:"breaking_changes"`
	DeprecatedFeatures   []string         `json:"deprecated_features"`
	RequiredCapabilities []string         `json:"required_capabilities"`
}

// BreakingChange represents a version that introduced breaking changes
type BreakingChange struct {
	Version     *Version `json:"version"`
	Description string   `json:"description"`
	Impact      string   `json:"impact"`     // "critical", "major", "minor"
	Mitigation  string   `json:"mitigation"` // How to handle the change
}

// CompatibilityResult represents the result of a compatibility check
type CompatibilityResult struct {
	IsCompatible       bool     `json:"is_compatible"`
	CompatibilityLevel string   `json:"compatibility_level"` // "full", "partial", "none"
	Warnings           []string `json:"warnings"`
	Errors             []string `json:"errors"`
	Recommendations    []string `json:"recommendations"`
	UpgradeRequired    bool     `json:"upgrade_required"`
	DowngradeRequired  bool     `json:"downgrade_required"`
}

// Predefined compatibility matrices for different service types
var (
	// zzRouter Node-to-Node compatibility (cluster communication).
	//
	// MinSupportedVersion here is INFORMATIONAL — surfaced in version
	// display and /health output so operators can see a human-readable
	// floor. It does NOT gate cluster pairing or mesh health; that job
	// belongs to CheckClusterProtocol and the cluster_protocol / min_
	// cluster_protocol ints in VersionInfo. Keep this value in rough
	// lockstep with release notes.
	ClusterCompatibility = &CompatibilityMatrix{
		MinSupportedVersion: MustParseVersion("0.1.0"),
		BreakingChanges:     []BreakingChange{
			// No breaking changes in 0.x series yet
		},
		RequiredCapabilities: ClusterRequiredCapabilities,
	}

	// zzRouter Client-to-Node compatibility (admin API)
	ClientCompatibility = &CompatibilityMatrix{
		MinSupportedVersion: MustParseVersion("0.1.0"),
		MaxSupportedVersion: MustParseVersion("1.0.0"),
		BreakingChanges:     []BreakingChange{
			// No breaking changes in 0.x series yet
		},
		RequiredCapabilities: []string{CapabilityAdminAPI},
	}

	// Provider compatibility (Ollama, vLLM, etc.)
	ProviderCompatibility = map[string]*CompatibilityMatrix{
		"ollama": {
			MinSupportedVersion: MustParseVersion("0.1.26"),
			BreakingChanges: []BreakingChange{
				{
					Version:     MustParseVersion("0.2.0"),
					Description: "New API endpoint structure",
					Impact:      "major",
					Mitigation:  "Update provider integration code",
				},
			},
		},
		"vllm": {
			MinSupportedVersion: MustParseVersion("0.3.0"),
			BreakingChanges: []BreakingChange{
				{
					Version:     MustParseVersion("0.4.0"),
					Description: "OpenAI API compatibility changes",
					Impact:      "minor",
					Mitigation:  "Update API endpoint mappings",
				},
			},
		},
	}
)

// CheckCompatibility performs comprehensive compatibility checking
func CheckCompatibility(local, remote *VersionInfo, matrix *CompatibilityMatrix) *CompatibilityResult {
	result := &CompatibilityResult{
		IsCompatible:       true,
		CompatibilityLevel: "full",
		Warnings:           []string{},
		Errors:             []string{},
		Recommendations:    []string{},
	}

	// Check minimum version requirement
	if remote.Version.IsLessThan(matrix.MinSupportedVersion) {
		result.IsCompatible = false
		result.CompatibilityLevel = "none"
		result.UpgradeRequired = true
		result.Errors = append(result.Errors,
			fmt.Sprintf("Remote version %s is below minimum supported %s",
				remote.Version.String(), matrix.MinSupportedVersion.String()))
		result.Recommendations = append(result.Recommendations,
			fmt.Sprintf("Upgrade remote service to version %s or later",
				matrix.MinSupportedVersion.String()))
	}

	// Check maximum version (future compatibility)
	if matrix.MaxSupportedVersion != nil && remote.Version.IsGreaterThan(matrix.MaxSupportedVersion) {
		result.CompatibilityLevel = "partial"
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Remote version %s is newer than maximum tested %s - compatibility not guaranteed",
				remote.Version.String(), matrix.MaxSupportedVersion.String()))
		result.Recommendations = append(result.Recommendations,
			"Consider upgrading local service to support newer remote versions")
	}

	// Check for breaking changes between versions
	breakingChanges := findBreakingChangesBetween(local.Version, remote.Version, matrix.BreakingChanges)
	for _, change := range breakingChanges {
		switch change.Impact {
		case "critical":
			result.IsCompatible = false
			result.CompatibilityLevel = "none"
			result.Errors = append(result.Errors,
				fmt.Sprintf("Critical breaking change in %s: %s",
					change.Version.String(), change.Description))
		case "major":
			result.CompatibilityLevel = "partial"
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Major breaking change in %s: %s",
					change.Version.String(), change.Description))
		case "minor":
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Minor breaking change in %s: %s",
					change.Version.String(), change.Description))
		}

		if change.Mitigation != "" {
			result.Recommendations = append(result.Recommendations, change.Mitigation)
		}
	}

	// Check required capabilities
	missingCapabilities := findMissingCapabilities(remote.Capabilities, matrix.RequiredCapabilities)
	if len(missingCapabilities) > 0 {
		result.IsCompatible = false
		result.CompatibilityLevel = "none"
		result.Errors = append(result.Errors,
			fmt.Sprintf("Remote service missing required capabilities: %s",
				strings.Join(missingCapabilities, ", ")))
		result.Recommendations = append(result.Recommendations,
			"Ensure remote service supports all required capabilities")
	}

	// Check major version compatibility
	if !local.Version.IsCompatibleWith(remote.Version) {
		result.CompatibilityLevel = "partial"
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Major version mismatch: local %d.x.x vs remote %d.x.x",
				local.Version.Major, remote.Version.Major))
	}

	return result
}

// findBreakingChangesBetween finds breaking changes that affect the version range
func findBreakingChangesBetween(localVersion, remoteVersion *Version, changes []BreakingChange) []BreakingChange {
	var relevantChanges []BreakingChange

	for _, change := range changes {
		// Check if the breaking change affects the compatibility between versions
		if (localVersion.IsLessThan(change.Version) && remoteVersion.IsGreaterThan(change.Version)) ||
			(remoteVersion.IsLessThan(change.Version) && localVersion.IsGreaterThan(change.Version)) {
			relevantChanges = append(relevantChanges, change)
		}
	}

	return relevantChanges
}

// findMissingCapabilities finds capabilities that are required but not present
func findMissingCapabilities(available, required []string) []string {
	availableSet := make(map[string]bool)
	for _, cap := range available {
		availableSet[cap] = true
	}

	var missing []string
	for _, req := range required {
		if !availableSet[req] {
			missing = append(missing, req)
		}
	}

	return missing
}

// CheckClusterProtocol enforces the coordinator's protocol-compat window
// against a joining or mesh-connected peer. Coordinator-side only; workers
// advertise their protocol numbers but do not gate the coordinator.
//
// Semantics (cap both ends):
//   - remoteProto == 0 → reject. A zero means the peer predates protocol
//     advertisement; no silent accept.
//   - remoteProto < localMin → reject "upgrade peer".
//   - remoteProto > localProto → reject "upgrade coordinator". A newer peer
//     may have reshaped a field this coordinator doesn't understand; silent
//     acceptance risks mis-parse.
//   - otherwise nil.
//
// No dev-build bypass: ClusterProtocolVersion is a source-level const and
// is populated in every build, including plain `go build` without ldflags.
func CheckClusterProtocol(localProto, localMin, remoteProto int) error {
	if remoteProto == 0 {
		return fmt.Errorf("peer did not advertise cluster_protocol (pre-protocol build); coordinator accepts v%d..v%d", localMin, localProto)
	}
	if remoteProto < localMin {
		return fmt.Errorf("peer cluster_protocol v%d is below coordinator minimum v%d; upgrade the peer", remoteProto, localMin)
	}
	if remoteProto > localProto {
		return fmt.Errorf("peer cluster_protocol v%d is above coordinator maximum v%d; upgrade the coordinator", remoteProto, localProto)
	}
	return nil
}

// GetCompatibilityMatrix returns the appropriate compatibility matrix for a connection type
func GetCompatibilityMatrix(connectionType string) *CompatibilityMatrix {
	switch connectionType {
	case CapabilityCluster, "host-to-host":
		return ClusterCompatibility
	case "client", "admin", "client-to-host":
		return ClientCompatibility
	default:
		// Default to client compatibility for unknown types
		return ClientCompatibility
	}
}
