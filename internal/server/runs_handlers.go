// package server — Runs shared types and helpers.
//
// Handler logic has been moved to:
//   - RunsController (runs_controller.go) — HTTP translation
//   - RunsUtilityService (runs_utility_service.go) — local utility queries
//   - RunsService (runs_service.go) — cluster-routed operations

package server

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// parseLaunchMode validates the launch mode string. Callers today only
// check the error return; the parsed value is kept in the signature so
// future callers that need the typed LaunchMode don't have to re-parse.
//
//nolint:unparam // returned LaunchMode is the public contract, not dead code
func parseLaunchMode(mode string) (instance.LaunchMode, error) {
	switch strings.ToLower(mode) {
	case string(instance.LaunchModeNative):
		return instance.LaunchModeNative, nil
	default:
		return "", fmt.Errorf("invalid launch mode: %s (must be 'native')", mode)
	}
}

// buildEnsureRequest creates an EnsureRequest from launch request parameters.
func buildEnsureRequest(providerName string, req LaunchRequest) prov_apps.EnsureRequest {
	return prov_apps.EnsureRequest{
		Provider:     providerName,
		Model:        req.Model,
		Port:         req.Port,
		Parameters:   req.Parameters,
		EnvVars:      req.EnvVars,
		Files:        req.Files,
		NativeConfig: req.NativeConfig,
	}
}

// LaunchRequest represents a request to launch an app instance.
type LaunchRequest struct {
	Model        string                 `json:"model_name,omitempty"`
	Provider     string                 `json:"provider" binding:"required"`
	LaunchMode   string                 `json:"launch_mode" binding:"required,oneof=native"`
	Port         int                    `json:"port,omitempty"`
	Files        []instance.FileMount   `json:"files,omitempty"`
	Parameters   map[string]string      `json:"parameters,omitempty"`
	EnvVars      map[string]string      `json:"env_vars,omitempty"`
	NativeConfig *instance.NativeConfig `json:"native_config,omitempty"`
}

// BatchLaunchRequest represents a batch launch request.
type BatchLaunchRequest struct {
	Instances []LaunchRequest `json:"instances" binding:"required"`
}
