package version

import (
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		want        *Version
		wantErr     bool
		errContains string
	}{
		{
			name:  "basic semantic version",
			input: "1.2.3",
			want: &Version{
				Major: 1,
				Minor: 2,
				Patch: 3,
			},
		},
		{
			name:  "with pre-release",
			input: "1.2.3-alpha",
			want: &Version{
				Major:      1,
				Minor:      2,
				Patch:      3,
				PreRelease: "alpha",
			},
		},
		{
			name:  "with build metadata",
			input: "1.2.3+build.123",
			want: &Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				BuildMeta: "build.123",
			},
		},
		{
			name:  "with pre-release and build metadata",
			input: "1.2.3-beta.1+commit.abc123",
			want: &Version{
				Major:      1,
				Minor:      2,
				Patch:      3,
				PreRelease: "beta.1",
				BuildMeta:  "commit.abc123",
			},
		},
		{
			name:  "version with leading zeros",
			input: "1.0.0",
			want: &Version{
				Major: 1,
				Minor: 0,
				Patch: 0,
			},
		},
		{
			name:  "high version numbers",
			input: "100.200.300",
			want: &Version{
				Major: 100,
				Minor: 200,
				Patch: 300,
			},
		},
		{
			name:  "complex pre-release",
			input: "1.0.0-alpha.beta.gamma",
			want: &Version{
				Major:      1,
				Minor:      0,
				Patch:      0,
				PreRelease: "alpha.beta.gamma",
			},
		},
		{
			name:  "version with spaces (trimmed)",
			input: "  1.2.3  ",
			want: &Version{
				Major: 1,
				Minor: 2,
				Patch: 3,
			},
		},
		{
			name:        "invalid format - no dots",
			input:       "123",
			wantErr:     true,
			errContains: "invalid version format",
		},
		{
			name:        "invalid format - missing patch",
			input:       "1.2",
			wantErr:     true,
			errContains: "invalid version format",
		},
		{
			name:        "invalid format - non-numeric",
			input:       "a.b.c",
			wantErr:     true,
			errContains: "invalid version format",
		},
		{
			name:        "empty string",
			input:       "",
			wantErr:     true,
			errContains: "invalid version format",
		},
		{
			name:        "invalid major version",
			input:       "x.2.3",
			wantErr:     true,
			errContains: "invalid version format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVersion(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseVersion() expected error, got nil")
					return
				}
				if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("ParseVersion() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseVersion() unexpected error = %v", err)
				return
			}

			if got.Major != tt.want.Major {
				t.Errorf("ParseVersion() Major = %v, want %v", got.Major, tt.want.Major)
			}
			if got.Minor != tt.want.Minor {
				t.Errorf("ParseVersion() Minor = %v, want %v", got.Minor, tt.want.Minor)
			}
			if got.Patch != tt.want.Patch {
				t.Errorf("ParseVersion() Patch = %v, want %v", got.Patch, tt.want.Patch)
			}
			if got.PreRelease != tt.want.PreRelease {
				t.Errorf("ParseVersion() PreRelease = %v, want %v", got.PreRelease, tt.want.PreRelease)
			}
			if got.BuildMeta != tt.want.BuildMeta {
				t.Errorf("ParseVersion() BuildMeta = %v, want %v", got.BuildMeta, tt.want.BuildMeta)
			}
		})
	}
}

func TestMustParseVersion(t *testing.T) {
	t.Run("valid version", func(t *testing.T) {
		// Should not panic
		v := MustParseVersion("1.2.3")
		if v.Major != 1 || v.Minor != 2 || v.Patch != 3 {
			t.Errorf("MustParseVersion() got %v, want 1.2.3", v)
		}
	})

	t.Run("invalid version panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("MustParseVersion() should panic on invalid version")
			}
		}()
		MustParseVersion("invalid")
	})
}

func TestVersion_String(t *testing.T) {
	tests := []struct {
		name    string
		version *Version
		want    string
	}{
		{
			name:    "basic version",
			version: &Version{Major: 1, Minor: 2, Patch: 3},
			want:    "1.2.3",
		},
		{
			name:    "with pre-release",
			version: &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "alpha"},
			want:    "1.2.3-alpha",
		},
		{
			name:    "with build metadata",
			version: &Version{Major: 1, Minor: 2, Patch: 3, BuildMeta: "build.123"},
			want:    "1.2.3+build.123",
		},
		{
			name:    "with pre-release and build metadata",
			version: &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "rc1", BuildMeta: "abc"},
			want:    "1.2.3-rc1+abc",
		},
		{
			name:    "zero version",
			version: &Version{Major: 0, Minor: 0, Patch: 0},
			want:    "0.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.version.String()
			if got != tt.want {
				t.Errorf("Version.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVersion_ShortString(t *testing.T) {
	tests := []struct {
		name    string
		version *Version
		want    string
	}{
		{
			name:    "basic version",
			version: &Version{Major: 1, Minor: 2, Patch: 3},
			want:    "1.2.3",
		},
		{
			name:    "ignores pre-release",
			version: &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "alpha"},
			want:    "1.2.3",
		},
		{
			name:    "ignores build metadata",
			version: &Version{Major: 1, Minor: 2, Patch: 3, BuildMeta: "build.123"},
			want:    "1.2.3",
		},
		{
			name:    "ignores both",
			version: &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "rc1", BuildMeta: "abc"},
			want:    "1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.version.ShortString()
			if got != tt.want {
				t.Errorf("Version.ShortString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVersion_IsLessThan(t *testing.T) {
	tests := []struct {
		name string
		v1   *Version
		v2   *Version
		want bool
	}{
		{
			name: "lower major version",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0},
			v2:   &Version{Major: 2, Minor: 0, Patch: 0},
			want: true,
		},
		{
			name: "same major, lower minor",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0},
			v2:   &Version{Major: 1, Minor: 1, Patch: 0},
			want: true,
		},
		{
			name: "same major and minor, lower patch",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0},
			v2:   &Version{Major: 1, Minor: 0, Patch: 1},
			want: true,
		},
		{
			name: "equal versions",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 1, Minor: 2, Patch: 3},
			want: false,
		},
		{
			name: "higher major version",
			v1:   &Version{Major: 2, Minor: 0, Patch: 0},
			v2:   &Version{Major: 1, Minor: 0, Patch: 0},
			want: false,
		},
		{
			name: "pre-release is less than release",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0, PreRelease: "alpha"},
			v2:   &Version{Major: 1, Minor: 0, Patch: 0},
			want: true,
		},
		{
			name: "release is not less than pre-release",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0},
			v2:   &Version{Major: 1, Minor: 0, Patch: 0, PreRelease: "alpha"},
			want: false,
		},
		{
			name: "pre-release lexical comparison",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0, PreRelease: "alpha"},
			v2:   &Version{Major: 1, Minor: 0, Patch: 0, PreRelease: "beta"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.v1.IsLessThan(tt.v2)
			if got != tt.want {
				t.Errorf("IsLessThan() = %v, want %v (v1=%s, v2=%s)", got, tt.want, tt.v1.String(), tt.v2.String())
			}
		})
	}
}

func TestVersion_IsGreaterThan(t *testing.T) {
	tests := []struct {
		name string
		v1   *Version
		v2   *Version
		want bool
	}{
		{
			name: "higher major version",
			v1:   &Version{Major: 2, Minor: 0, Patch: 0},
			v2:   &Version{Major: 1, Minor: 0, Patch: 0},
			want: true,
		},
		{
			name: "same major, higher minor",
			v1:   &Version{Major: 1, Minor: 1, Patch: 0},
			v2:   &Version{Major: 1, Minor: 0, Patch: 0},
			want: true,
		},
		{
			name: "equal versions",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 1, Minor: 2, Patch: 3},
			want: false,
		},
		{
			name: "lower version",
			v1:   &Version{Major: 1, Minor: 0, Patch: 0},
			v2:   &Version{Major: 2, Minor: 0, Patch: 0},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.v1.IsGreaterThan(tt.v2)
			if got != tt.want {
				t.Errorf("IsGreaterThan() = %v, want %v (v1=%s, v2=%s)", got, tt.want, tt.v1.String(), tt.v2.String())
			}
		})
	}
}

func TestVersion_IsEqual(t *testing.T) {
	tests := []struct {
		name string
		v1   *Version
		v2   *Version
		want bool
	}{
		{
			name: "equal versions",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 1, Minor: 2, Patch: 3},
			want: true,
		},
		{
			name: "different major",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 2, Minor: 2, Patch: 3},
			want: false,
		},
		{
			name: "different minor",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 1, Minor: 3, Patch: 3},
			want: false,
		},
		{
			name: "different patch",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 1, Minor: 2, Patch: 4},
			want: false,
		},
		{
			name: "different pre-release",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "alpha"},
			v2:   &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "beta"},
			want: false,
		},
		{
			name: "equal with pre-release",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "alpha"},
			v2:   &Version{Major: 1, Minor: 2, Patch: 3, PreRelease: "alpha"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.v1.IsEqual(tt.v2)
			if got != tt.want {
				t.Errorf("IsEqual() = %v, want %v (v1=%s, v2=%s)", got, tt.want, tt.v1.String(), tt.v2.String())
			}
		})
	}
}

func TestVersion_IsCompatibleWith(t *testing.T) {
	tests := []struct {
		name string
		v1   *Version
		v2   *Version
		want bool
	}{
		{
			name: "same major version",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 1, Minor: 5, Patch: 0},
			want: true,
		},
		{
			name: "different major version",
			v1:   &Version{Major: 1, Minor: 2, Patch: 3},
			v2:   &Version{Major: 2, Minor: 0, Patch: 0},
			want: false,
		},
		{
			name: "both version 0",
			v1:   &Version{Major: 0, Minor: 1, Patch: 0},
			v2:   &Version{Major: 0, Minor: 2, Patch: 0},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.v1.IsCompatibleWith(tt.v2)
			if got != tt.want {
				t.Errorf("IsCompatibleWith() = %v, want %v (v1=%s, v2=%s)", got, tt.want, tt.v1.String(), tt.v2.String())
			}
		})
	}
}

func TestGetCurrentVersionInfo(t *testing.T) {
	serviceType := "test-service"
	capabilities := []string{"cap1", "cap2"}

	info := GetCurrentVersionInfo(serviceType, capabilities)

	if info == nil {
		t.Fatal("GetCurrentVersionInfo() returned nil")
	}

	if info.Version == nil {
		t.Error("Version is nil")
	}

	if info.BuildInfo == nil {
		t.Error("BuildInfo is nil")
	}

	if info.ServiceType != serviceType {
		t.Errorf("ServiceType = %v, want %v", info.ServiceType, serviceType)
	}

	if len(info.Capabilities) != len(capabilities) {
		t.Errorf("len(Capabilities) = %v, want %v", len(info.Capabilities), len(capabilities))
	}

	if info.APIVersion != "v1" {
		t.Errorf("APIVersion = %v, want v1", info.APIVersion)
	}
}

func TestInit(t *testing.T) {
	// Test that Current version is initialized
	if Current == nil {
		t.Fatal("Current version is nil after init")
	}

	// Should have valid version numbers (at least 0)
	if Current.Major < 0 {
		t.Errorf("Current.Major = %v, want >= 0", Current.Major)
	}
	if Current.Minor < 0 {
		t.Errorf("Current.Minor = %v, want >= 0", Current.Minor)
	}
	if Current.Patch < 0 {
		t.Errorf("Current.Patch = %v, want >= 0", Current.Patch)
	}
}

func TestVersion_RoundTrip(t *testing.T) {
	// Test that parsing a version string and converting back gives the same string
	tests := []string{
		"1.2.3",
		"1.2.3-alpha",
		"1.2.3+build.123",
		"1.2.3-beta.1+commit.abc",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			v, err := ParseVersion(tt)
			if err != nil {
				t.Fatalf("ParseVersion() error = %v", err)
			}

			got := v.String()
			if got != tt {
				t.Errorf("Round trip failed: input = %v, output = %v", tt, got)
			}
		})
	}
}

func TestBuildInfo_Fields(t *testing.T) {
	info := GetCurrentVersionInfo("test", []string{})

	// Verify BuildInfo has all fields populated
	buildInfo := info.BuildInfo
	if buildInfo == nil {
		t.Fatal("BuildInfo is nil")
	}

	// Fields should at least be initialized (may be "unknown" at build time)
	if buildInfo.GitCommit == "" {
		t.Error("GitCommit is empty")
	}
	if buildInfo.GitBranch == "" {
		t.Error("GitBranch is empty")
	}
	if buildInfo.GoVersion == "" {
		t.Error("GoVersion is empty")
	}
	if buildInfo.Platform == "" {
		t.Error("Platform is empty")
	}
	if buildInfo.BuildUser == "" {
		t.Error("BuildUser is empty")
	}

	// BuildDate might be zero if parsing fails, that's okay
	_ = buildInfo.BuildDate
}

func TestBuildInfo_DateParsing(t *testing.T) {
	// Test with a valid RFC3339 date
	oldBuildDate := BuildDate
	defer func() { BuildDate = oldBuildDate }()

	BuildDate = "2024-01-15T10:30:00Z"
	info := GetCurrentVersionInfo("test", []string{})

	if info.BuildInfo.BuildDate.IsZero() {
		t.Error("BuildDate should be parsed and not zero")
	}

	expectedTime, _ := time.Parse(time.RFC3339, "2024-01-15T10:30:00Z")
	if !info.BuildInfo.BuildDate.Equal(expectedTime) {
		t.Errorf("BuildDate = %v, want %v", info.BuildInfo.BuildDate, expectedTime)
	}
}

// Helper function
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
