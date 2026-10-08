package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Version represents a semantic version following semver.org specification
type Version struct {
	Major      int    `json:"major"`
	Minor      int    `json:"minor"`
	Patch      int    `json:"patch"`
	PreRelease string `json:"pre_release,omitempty"` // alpha, beta, rc1, etc.
	BuildMeta  string `json:"build_meta,omitempty"`  // git commit, build date, etc.
}

// BuildInfo contains build-time information
type BuildInfo struct {
	GitCommit string    `json:"git_commit"`
	GitBranch string    `json:"git_branch"`
	BuildDate time.Time `json:"build_date"`
	GoVersion string    `json:"go_version"`
	Platform  string    `json:"platform"`
	BuildUser string    `json:"build_user"`
}

// VersionInfo represents complete version information for a service
type VersionInfo struct {
	Version      *Version   `json:"version"`
	BuildInfo    *BuildInfo `json:"build_info"`
	APIVersion   string     `json:"api_version"`
	Capabilities []string   `json:"capabilities"`
	ServiceType  string     `json:"service_type"` // "zzrouter-host", "client", "ollama", "vllm"

	// ClusterProtocol is this build's cluster wire contract version. The
	// coordinator gates pairing + mesh health on this (see CheckClusterProtocol).
	// A zero value from a peer means it predates protocol advertisement and is rejected.
	ClusterProtocol int `json:"cluster_protocol"`
	// MinClusterProtocol is the oldest peer protocol this build accepts.
	// Advertised by both peers; only the coordinator gates.
	MinClusterProtocol int `json:"min_cluster_protocol"`
}

// Service type constants for consistent usage across the codebase
const (
	ServiceTypeHost   = "zzrouter-host" // Server/host service
	ServiceTypeNode   = "zzrouter-node" // Node binary
	ServiceTypeClient = "client"        // CLI client

	// Version constants
	UnknownVersion = "unknown"
	DevVersion     = "dev"
	ZeroVersion    = "0.0.0"
)

// Build-time variables populated by the Makefile via `-ldflags "-X ..."`.
// A plain `go build` (without the Makefile) leaves these empty, which yields
// a clearly-marked dev version like "0.0.0-dev" instead of a plausible-but-
// wrong real version. The single source of truth is the git tag; the Makefile
// runs `git describe --tags --abbrev=0` and splits it into the components
// below. Never hard-code real version numbers here.
var (
	GitCommit = "unknown"
	GitBranch = "unknown"
	BuildDate = "unknown"
	GoVersion = "unknown"
	Platform  = "unknown"
	BuildUser = "unknown"

	Major      = ""
	Minor      = ""
	Patch      = ""
	PreRelease = ""

	Current *Version
)

// Initialize current version from build-time variables
func init() {
	major, _ := strconv.Atoi(Major)
	minor, _ := strconv.Atoi(Minor)
	patch, _ := strconv.Atoi(Patch)

	preRelease := PreRelease
	if Major == "" && Minor == "" && Patch == "" && preRelease == "" {
		preRelease = "dev"
	}

	// Encode build time as MMDDHHMM in UTC so the build metadata is identical
	// regardless of where (or in which timezone) the binary was built.
	buildMeta := ""
	if t, err := time.Parse(time.RFC3339, BuildDate); err == nil {
		buildMeta = t.UTC().Format(utils.LayoutBuildMeta)
	}

	Current = &Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		PreRelease: preRelease,
		BuildMeta:  buildMeta,
	}
}

// String returns the version as a string (e.g., "1.2.3.03281220" or "1.2.3-alpha.03281220")
func (v *Version) String() string {
	version := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)

	if v.PreRelease != "" {
		version += "-" + v.PreRelease
	}

	if v.BuildMeta != "" {
		version += "+" + v.BuildMeta
	}

	return version
}

// ShortString returns a short version string (e.g., "1.2.3")
func (v *Version) ShortString() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// IsLessThan compares versions (semantic version comparison)
func (v *Version) IsLessThan(other *Version) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	if v.Patch != other.Patch {
		return v.Patch < other.Patch
	}

	// Pre-release versions have lower precedence than normal versions
	if v.PreRelease == "" && other.PreRelease != "" {
		return false
	}
	if v.PreRelease != "" && other.PreRelease == "" {
		return true
	}

	// Compare pre-release versions lexically
	return v.PreRelease < other.PreRelease
}

// IsGreaterThan compares versions
func (v *Version) IsGreaterThan(other *Version) bool {
	return other.IsLessThan(v)
}

// IsEqual compares versions for equality
func (v *Version) IsEqual(other *Version) bool {
	return !v.IsLessThan(other) && !v.IsGreaterThan(other)
}

// IsCompatibleWith checks if versions are compatible (same major version)
func (v *Version) IsCompatibleWith(other *Version) bool {
	return v.Major == other.Major
}

// ParseVersion parses a version string into a Version struct
func ParseVersion(versionStr string) (*Version, error) {
	// Regex for semantic version: major.minor.patch[-prerelease][+buildmeta]
	re := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([a-zA-Z0-9\-\.]+))?(?:\+([a-zA-Z0-9\-\.]+))?$`)

	matches := re.FindStringSubmatch(strings.TrimSpace(versionStr))
	if matches == nil {
		return nil, fmt.Errorf("invalid version format: %s", versionStr)
	}

	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return nil, fmt.Errorf("invalid major version: %s", matches[1])
	}

	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return nil, fmt.Errorf("invalid minor version: %s", matches[2])
	}

	patch, err := strconv.Atoi(matches[3])
	if err != nil {
		return nil, fmt.Errorf("invalid patch version: %s", matches[3])
	}

	return &Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		PreRelease: matches[4], // Can be empty
		BuildMeta:  matches[5], // Can be empty
	}, nil
}

// MustParseVersion parses a version string and panics on error.
// This function is intended ONLY for compile-time constants where the
// version string is known to be valid (e.g., defining compatibility matrices).
// DO NOT use this function with user-provided or runtime-determined version strings.
// For runtime parsing, use ParseVersion which returns an error.
func MustParseVersion(versionStr string) *Version {
	v, err := ParseVersion(versionStr)
	if err != nil {
		panic(fmt.Sprintf("invalid version: %s", err))
	}
	return v
}

// GetCurrentVersionInfo returns complete version information for this service
func GetCurrentVersionInfo(serviceType string, capabilities []string) *VersionInfo {
	buildDate, _ := time.Parse(time.RFC3339, BuildDate)

	return &VersionInfo{
		Version: Current,
		BuildInfo: &BuildInfo{
			GitCommit: GitCommit,
			GitBranch: GitBranch,
			BuildDate: buildDate,
			GoVersion: GoVersion,
			Platform:  Platform,
			BuildUser: BuildUser,
		},
		APIVersion:         "v1", // Current API version
		Capabilities:       capabilities,
		ServiceType:        serviceType,
		ClusterProtocol:    ClusterProtocolVersion,
		MinClusterProtocol: MinClusterProtocolVersion,
	}
}
