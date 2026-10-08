package version

import (
	"slices"
	"testing"
)

func TestCheckCompatibility(t *testing.T) {
	tests := []struct {
		name                  string
		localVersion          *VersionInfo
		remoteVersion         *VersionInfo
		matrix                *CompatibilityMatrix
		wantCompatible        bool
		wantLevel             string
		wantErrorCount        int
		wantWarningCount      int
		wantUpgradeRequired   bool
		wantDowngradeRequired bool
	}{
		{
			name: "compatible same version",
			localVersion: &VersionInfo{
				Version: &Version{Major: 1, Minor: 0, Patch: 0},
			},
			remoteVersion: &VersionInfo{
				Version: &Version{Major: 1, Minor: 0, Patch: 0},
			},
			matrix: &CompatibilityMatrix{
				MinSupportedVersion: &Version{Major: 0, Minor: 1, Patch: 0},
			},
			wantCompatible:   true,
			wantLevel:        "full",
			wantErrorCount:   0,
			wantWarningCount: 0,
		},
		{
			name: "remote below minimum",
			localVersion: &VersionInfo{
				Version: &Version{Major: 1, Minor: 0, Patch: 0},
			},
			remoteVersion: &VersionInfo{
				Version: &Version{Major: 0, Minor: 0, Patch: 1},
			},
			matrix: &CompatibilityMatrix{
				MinSupportedVersion: &Version{Major: 0, Minor: 5, Patch: 0},
			},
			wantCompatible:      false,
			wantLevel:           "partial", // Gets partial due to major version mismatch check overriding
			wantErrorCount:      1,
			wantWarningCount:    1,
			wantUpgradeRequired: true,
		},
		{
			name: "remote above maximum",
			localVersion: &VersionInfo{
				Version: &Version{Major: 1, Minor: 0, Patch: 0},
			},
			remoteVersion: &VersionInfo{
				Version: &Version{Major: 2, Minor: 0, Patch: 0},
			},
			matrix: &CompatibilityMatrix{
				MinSupportedVersion: &Version{Major: 0, Minor: 1, Patch: 0},
				MaxSupportedVersion: &Version{Major: 1, Minor: 5, Patch: 0},
			},
			wantCompatible:   true,
			wantLevel:        "partial",
			wantWarningCount: 2, // Both max version and major version warnings
		},
		{
			name: "major version mismatch",
			localVersion: &VersionInfo{
				Version: &Version{Major: 1, Minor: 0, Patch: 0},
			},
			remoteVersion: &VersionInfo{
				Version: &Version{Major: 2, Minor: 0, Patch: 0},
			},
			matrix: &CompatibilityMatrix{
				MinSupportedVersion: &Version{Major: 0, Minor: 1, Patch: 0},
			},
			wantCompatible:   true,
			wantLevel:        "partial",
			wantWarningCount: 1,
		},
		{
			name: "missing required capabilities",
			localVersion: &VersionInfo{
				Version:      &Version{Major: 1, Minor: 0, Patch: 0},
				Capabilities: []string{"feature1", "feature2"},
			},
			remoteVersion: &VersionInfo{
				Version:      &Version{Major: 1, Minor: 0, Patch: 0},
				Capabilities: []string{"feature1"},
			},
			matrix: &CompatibilityMatrix{
				MinSupportedVersion:  &Version{Major: 0, Minor: 1, Patch: 0},
				RequiredCapabilities: []string{"feature1", "feature2", "feature3"},
			},
			wantCompatible: false,
			wantLevel:      "none",
			wantErrorCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckCompatibility(tt.localVersion, tt.remoteVersion, tt.matrix)

			if result.IsCompatible != tt.wantCompatible {
				t.Errorf("IsCompatible = %v, want %v", result.IsCompatible, tt.wantCompatible)
			}

			if result.CompatibilityLevel != tt.wantLevel {
				t.Errorf("CompatibilityLevel = %v, want %v", result.CompatibilityLevel, tt.wantLevel)
			}

			if len(result.Errors) != tt.wantErrorCount {
				t.Errorf("Error count = %v, want %v. Errors: %v", len(result.Errors), tt.wantErrorCount, result.Errors)
			}

			if len(result.Warnings) != tt.wantWarningCount {
				t.Errorf("Warning count = %v, want %v. Warnings: %v", len(result.Warnings), tt.wantWarningCount, result.Warnings)
			}

			if result.UpgradeRequired != tt.wantUpgradeRequired {
				t.Errorf("UpgradeRequired = %v, want %v", result.UpgradeRequired, tt.wantUpgradeRequired)
			}

			if result.DowngradeRequired != tt.wantDowngradeRequired {
				t.Errorf("DowngradeRequired = %v, want %v", result.DowngradeRequired, tt.wantDowngradeRequired)
			}
		})
	}
}

func TestCheckCompatibility_WithBreakingChanges(t *testing.T) {
	breakingChange := BreakingChange{
		Version:     &Version{Major: 1, Minor: 5, Patch: 0},
		Description: "API restructure",
		Impact:      "critical",
		Mitigation:  "Update your client",
	}

	tests := []struct {
		name           string
		localVersion   *Version
		remoteVersion  *Version
		changes        []BreakingChange
		wantCompatible bool
		wantLevel      string
	}{
		{
			name:           "breaking change between versions - critical",
			localVersion:   &Version{Major: 1, Minor: 0, Patch: 0},
			remoteVersion:  &Version{Major: 1, Minor: 6, Patch: 0},
			changes:        []BreakingChange{breakingChange},
			wantCompatible: false,
			wantLevel:      "none",
		},
		{
			name:           "both versions before breaking change",
			localVersion:   &Version{Major: 1, Minor: 0, Patch: 0},
			remoteVersion:  &Version{Major: 1, Minor: 2, Patch: 0},
			changes:        []BreakingChange{breakingChange},
			wantCompatible: true,
			wantLevel:      "full",
		},
		{
			name:           "both versions after breaking change",
			localVersion:   &Version{Major: 1, Minor: 6, Patch: 0},
			remoteVersion:  &Version{Major: 1, Minor: 7, Patch: 0},
			changes:        []BreakingChange{breakingChange},
			wantCompatible: true,
			wantLevel:      "full",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matrix := &CompatibilityMatrix{
				MinSupportedVersion: &Version{Major: 0, Minor: 1, Patch: 0},
				BreakingChanges:     tt.changes,
			}

			local := &VersionInfo{Version: tt.localVersion}
			remote := &VersionInfo{Version: tt.remoteVersion}

			result := CheckCompatibility(local, remote, matrix)

			if result.IsCompatible != tt.wantCompatible {
				t.Errorf("IsCompatible = %v, want %v", result.IsCompatible, tt.wantCompatible)
			}

			if result.CompatibilityLevel != tt.wantLevel {
				t.Errorf("CompatibilityLevel = %v, want %v", result.CompatibilityLevel, tt.wantLevel)
			}
		})
	}
}

func TestCheckCompatibility_BreakingChangeImpact(t *testing.T) {
	tests := []struct {
		name           string
		impact         string
		wantCompatible bool
		wantLevel      string
	}{
		{
			name:           "critical impact",
			impact:         "critical",
			wantCompatible: false,
			wantLevel:      "none",
		},
		{
			name:           "major impact",
			impact:         "major",
			wantCompatible: true,
			wantLevel:      "partial",
		},
		{
			name:           "minor impact",
			impact:         "minor",
			wantCompatible: true,
			wantLevel:      "full",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			change := BreakingChange{
				Version:     &Version{Major: 1, Minor: 5, Patch: 0},
				Description: "Test change",
				Impact:      tt.impact,
				Mitigation:  "Test mitigation",
			}

			matrix := &CompatibilityMatrix{
				MinSupportedVersion: &Version{Major: 0, Minor: 1, Patch: 0},
				BreakingChanges:     []BreakingChange{change},
			}

			local := &VersionInfo{Version: &Version{Major: 1, Minor: 0, Patch: 0}}
			remote := &VersionInfo{Version: &Version{Major: 1, Minor: 6, Patch: 0}}

			result := CheckCompatibility(local, remote, matrix)

			if result.IsCompatible != tt.wantCompatible {
				t.Errorf("IsCompatible = %v, want %v", result.IsCompatible, tt.wantCompatible)
			}

			if result.CompatibilityLevel != tt.wantLevel {
				t.Errorf("CompatibilityLevel = %v, want %v", result.CompatibilityLevel, tt.wantLevel)
			}

			// Check that recommendations include mitigation
			if tt.impact == "critical" || tt.impact == "major" {
				foundMitigation := slices.Contains(result.Recommendations, "Test mitigation")
				if !foundMitigation {
					t.Error("Expected mitigation in recommendations")
				}
			}
		})
	}
}

func TestFindBreakingChangesBetween(t *testing.T) {
	change1 := BreakingChange{
		Version:     &Version{Major: 1, Minor: 5, Patch: 0},
		Description: "Change at 1.5.0",
	}
	change2 := BreakingChange{
		Version:     &Version{Major: 2, Minor: 0, Patch: 0},
		Description: "Change at 2.0.0",
	}

	allChanges := []BreakingChange{change1, change2}

	tests := []struct {
		name          string
		localVersion  *Version
		remoteVersion *Version
		wantCount     int
	}{
		{
			name:          "no breaking changes between",
			localVersion:  &Version{Major: 1, Minor: 0, Patch: 0},
			remoteVersion: &Version{Major: 1, Minor: 2, Patch: 0},
			wantCount:     0,
		},
		{
			name:          "one breaking change between",
			localVersion:  &Version{Major: 1, Minor: 0, Patch: 0},
			remoteVersion: &Version{Major: 1, Minor: 6, Patch: 0},
			wantCount:     1,
		},
		{
			name:          "two breaking changes between",
			localVersion:  &Version{Major: 1, Minor: 0, Patch: 0},
			remoteVersion: &Version{Major: 2, Minor: 1, Patch: 0},
			wantCount:     2,
		},
		{
			name:          "both after all changes",
			localVersion:  &Version{Major: 2, Minor: 1, Patch: 0},
			remoteVersion: &Version{Major: 2, Minor: 2, Patch: 0},
			wantCount:     0,
		},
		{
			name:          "reversed order (remote older)",
			localVersion:  &Version{Major: 2, Minor: 0, Patch: 0},
			remoteVersion: &Version{Major: 1, Minor: 0, Patch: 0},
			wantCount:     1, // Only the change at 2.0.0 affects this range
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findBreakingChangesBetween(tt.localVersion, tt.remoteVersion, allChanges)

			if len(result) != tt.wantCount {
				t.Errorf("findBreakingChangesBetween() returned %d changes, want %d", len(result), tt.wantCount)
			}
		})
	}
}

func TestFindMissingCapabilities(t *testing.T) {
	tests := []struct {
		name      string
		available []string
		required  []string
		want      []string
	}{
		{
			name:      "all capabilities present",
			available: []string{"cap1", "cap2", "cap3"},
			required:  []string{"cap1", "cap2"},
			want:      nil,
		},
		{
			name:      "one capability missing",
			available: []string{"cap1", "cap2"},
			required:  []string{"cap1", "cap2", "cap3"},
			want:      []string{"cap3"},
		},
		{
			name:      "multiple capabilities missing",
			available: []string{"cap1"},
			required:  []string{"cap1", "cap2", "cap3"},
			want:      []string{"cap2", "cap3"},
		},
		{
			name:      "no capabilities available",
			available: []string{},
			required:  []string{"cap1", "cap2"},
			want:      []string{"cap1", "cap2"},
		},
		{
			name:      "no capabilities required",
			available: []string{"cap1", "cap2"},
			required:  []string{},
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findMissingCapabilities(tt.available, tt.required)

			if len(result) != len(tt.want) {
				t.Errorf("findMissingCapabilities() returned %v, want %v", result, tt.want)
				return
			}

			if tt.want != nil {
				for i, cap := range result {
					if cap != tt.want[i] {
						t.Errorf("Missing capability at index %d: got %v, want %v", i, cap, tt.want[i])
					}
				}
			}
		})
	}
}

func TestGetCompatibilityMatrix(t *testing.T) {
	tests := []struct {
		name           string
		connectionType string
		wantMatrix     *CompatibilityMatrix
	}{
		{
			name:           "cluster connection",
			connectionType: CapabilityCluster,
			wantMatrix:     ClusterCompatibility,
		},
		{
			name:           "host-to-host connection",
			connectionType: "host-to-host",
			wantMatrix:     ClusterCompatibility,
		},
		{
			name:           "client connection",
			connectionType: "client",
			wantMatrix:     ClientCompatibility,
		},
		{
			name:           "admin connection",
			connectionType: "admin",
			wantMatrix:     ClientCompatibility,
		},
		{
			name:           "client-to-host connection",
			connectionType: "client-to-host",
			wantMatrix:     ClientCompatibility,
		},
		{
			name:           "unknown connection defaults to client",
			connectionType: "unknown",
			wantMatrix:     ClientCompatibility,
		},
		{
			name:           "empty connection type defaults to client",
			connectionType: "",
			wantMatrix:     ClientCompatibility,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetCompatibilityMatrix(tt.connectionType)

			if result != tt.wantMatrix {
				t.Errorf("GetCompatibilityMatrix(%q) returned different matrix than expected", tt.connectionType)
			}
		})
	}
}

func TestPredefinedCompatibilityMatrices(t *testing.T) {
	t.Run("ClusterCompatibility", func(t *testing.T) {
		if ClusterCompatibility == nil {
			t.Fatal("ClusterCompatibility is nil")
		}

		if ClusterCompatibility.MinSupportedVersion == nil {
			t.Error("MinSupportedVersion is nil")
		}

		// MaxSupportedVersion intentionally nil on ClusterCompatibility:
		// cluster gating moved to CheckClusterProtocol; the matrix is
		// informational. See pkg/version/protocol.go for the real gate.
		if ClusterCompatibility.MaxSupportedVersion != nil {
			t.Error("ClusterCompatibility.MaxSupportedVersion must be nil; gating moved to CheckClusterProtocol")
		}

		if len(ClusterCompatibility.RequiredCapabilities) == 0 {
			t.Error("RequiredCapabilities is empty")
		}
	})

	t.Run("ClientCompatibility", func(t *testing.T) {
		if ClientCompatibility == nil {
			t.Fatal("ClientCompatibility is nil")
		}

		if ClientCompatibility.MinSupportedVersion == nil {
			t.Error("MinSupportedVersion is nil")
		}

		if ClientCompatibility.MaxSupportedVersion == nil {
			t.Error("MaxSupportedVersion is nil")
		}

		if len(ClientCompatibility.RequiredCapabilities) == 0 {
			t.Error("RequiredCapabilities is empty")
		}
	})

	t.Run("ProviderCompatibility", func(t *testing.T) {
		if ProviderCompatibility == nil {
			t.Fatal("ProviderCompatibility is nil")
		}

		// Check ollama exists
		if _, exists := ProviderCompatibility["ollama"]; !exists {
			t.Error("ollama provider not found in ProviderCompatibility")
		}

		// Check vllm exists
		if _, exists := ProviderCompatibility["vllm"]; !exists {
			t.Error("vllm provider not found in ProviderCompatibility")
		}
	})
}

func TestCompatibilityResult_Fields(t *testing.T) {
	result := &CompatibilityResult{
		IsCompatible:       true,
		CompatibilityLevel: "full",
		Warnings:           []string{"warning1"},
		Errors:             []string{},
		Recommendations:    []string{"rec1", "rec2"},
		UpgradeRequired:    false,
		DowngradeRequired:  false,
	}

	if !result.IsCompatible {
		t.Error("IsCompatible should be true")
	}

	if result.CompatibilityLevel != "full" {
		t.Error("CompatibilityLevel should be full")
	}

	if len(result.Warnings) != 1 {
		t.Error("Should have 1 warning")
	}
	if len(result.Errors) != 0 {
		t.Error("Should have 0 errors")
	}
	if len(result.Recommendations) != 2 {
		t.Error("Should have 2 recommendations")
	}
	if result.UpgradeRequired {
		t.Error("UpgradeRequired should be false")
	}
	if result.DowngradeRequired {
		t.Error("DowngradeRequired should be false")
	}
}

// Benchmark tests
func BenchmarkCheckCompatibility(b *testing.B) {
	local := &VersionInfo{
		Version:      &Version{Major: 1, Minor: 0, Patch: 0},
		Capabilities: []string{"cap1", "cap2"},
	}
	remote := &VersionInfo{
		Version:      &Version{Major: 1, Minor: 2, Patch: 0},
		Capabilities: []string{"cap1", "cap2", "cap3"},
	}
	matrix := &CompatibilityMatrix{
		MinSupportedVersion:  &Version{Major: 0, Minor: 5, Patch: 0},
		MaxSupportedVersion:  &Version{Major: 2, Minor: 0, Patch: 0},
		RequiredCapabilities: []string{"cap1"},
	}

	for i := 0; i < b.N; i++ {
		CheckCompatibility(local, remote, matrix)
	}
}

func BenchmarkFindMissingCapabilities(b *testing.B) {
	available := []string{"cap1", "cap2", "cap3", "cap4", "cap5"}
	required := []string{"cap1", "cap3", "cap5", "cap7", "cap9"}

	for i := 0; i < b.N; i++ {
		findMissingCapabilities(available, required)
	}
}
