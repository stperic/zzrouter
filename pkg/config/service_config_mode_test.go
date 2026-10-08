package config

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/constants"
)

func TestServiceConfig_LaunchesProcess(t *testing.T) {
	tests := []struct {
		mode     string
		expected bool
	}{
		{constants.AppModeOnDemand, true},
		{constants.AppModeService, false},
		{constants.AppModeExternal, false},
		{constants.AppModeCloud, false},
	}
	for _, tt := range tests {
		sc := &ServiceConfig{Mode: tt.mode}
		if got := sc.LaunchesProcess(); got != tt.expected {
			t.Errorf("Mode %q: LaunchesProcess() = %v, want %v", tt.mode, got, tt.expected)
		}
	}
}

func TestServiceConfig_HasEndpoint(t *testing.T) {
	tests := []struct {
		mode     string
		expected bool
	}{
		{constants.AppModeOnDemand, false},
		{constants.AppModeService, true},
		{constants.AppModeExternal, true},
		{constants.AppModeCloud, true},
	}
	for _, tt := range tests {
		sc := &ServiceConfig{Mode: tt.mode}
		if got := sc.HasEndpoint(); got != tt.expected {
			t.Errorf("Mode %q: HasEndpoint() = %v, want %v", tt.mode, got, tt.expected)
		}
	}
}

func TestServiceConfig_ModeHelpers(t *testing.T) {
	sc := &ServiceConfig{Mode: constants.AppModeExternal}
	if sc.IsCloudProvider() {
		t.Error("external mode: IsCloudProvider() should be false")
	}
	if !sc.IsLocalProvider() {
		t.Error("external mode: IsLocalProvider() should be true")
	}

	sc.Mode = constants.AppModeCloud
	if !sc.IsCloudProvider() {
		t.Error("cloud mode: IsCloudProvider() should be true")
	}
	if sc.IsLocalProvider() {
		t.Error("cloud mode: IsLocalProvider() should be false")
	}
}

func TestServiceConfig_GetMetadata_BackwardCompat(t *testing.T) {
	// New style: metadata struct
	sc := &ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Metadata: &AppMetadata{
				ProcessorType: "GPU",
				ContextParam:  "max-model-len",
			},
		},
	}
	if sc.GetProcessorType() != "GPU" {
		t.Errorf("Expected 'GPU', got %q", sc.GetProcessorType())
	}
	if sc.GetContextParam() != "max-model-len" {
		t.Errorf("Expected 'max-model-len', got %q", sc.GetContextParam())
	}

	// No defaults
	sc3 := &ServiceConfig{}
	if sc3.GetMetadata() != nil {
		t.Error("No defaults: GetMetadata() should return nil")
	}
}
