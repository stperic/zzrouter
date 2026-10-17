// Package servicemanager provides an abstraction for managing system services.
// Used by external/service-mode providers (e.g., Ollama) where zzRouter can
// attempt to restart the service and apply environment variable changes.
//
// Design: try the operation, return instructions on permission failure.
// Never use sudo or escalate privileges.
package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

// ServiceStatus represents the current state of a managed service.
type ServiceStatus struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	Manager string `json:"manager"`
	// ObservationError means current process state could not be established.
	ObservationError error `json:"-"`
}

// ApplyResult contains the outcome of an apply + restart operation.
type ApplyResult struct {
	Applied      bool   `json:"applied"`
	Manager      string `json:"manager"`
	EnvVarsCount int    `json:"env_vars_applied,omitempty"`
	Restarted    bool   `json:"restarted"`
	Error        string `json:"error,omitempty"`
	Instructions string `json:"manual_instructions,omitempty"` // Commands to run manually if apply failed
	Warning      string `json:"warning,omitempty"`
}

// Manager is the interface for platform-specific service management.
type Manager interface {
	// Name returns the service manager identifier (systemd, launchd, nssm).
	Name() string

	// ApplyEnv writes environment variables and restarts the service.
	// On permission failure, returns an ApplyResult with Instructions instead of error.
	ApplyEnv(serviceName string, env map[string]string) *ApplyResult

	// Restart restarts the service without changing env vars.
	Restart(serviceName string) error

	// Status returns the current service status.
	Status(serviceName string) ServiceStatus

	// Instructions generates the manual commands for applying env vars.
	Instructions(serviceName string, env map[string]string) string
}

// Probe performs bounded service-manager observations.
type Probe interface {
	// DetectContext reports whether this manager owns the service; an error
	// means it could not tell, never that the service is absent.
	DetectContext(context.Context, string) (bool, error)
	StatusContext(context.Context, string) ServiceStatus
}

// EnvironmentController applies the configured runtime environment to an existing service.
type EnvironmentController interface {
	ApplyEnvContext(context.Context, string, map[string]string) *ApplyResult
}

// DetectContext detects existing ownership without creating host services.
// A nil Manager with a nil error means no host service manager owns it.
func DetectContext(ctx context.Context, serviceName string) (Manager, error) {
	for _, m := range platformManagers() {
		probe, ok := m.(Probe)
		if !ok {
			continue
		}
		detected, err := probe.DetectContext(ctx, serviceName)
		if err != nil {
			return nil, err
		}
		if detected {
			return m, nil
		}
	}
	return nil, nil
}

// probeErr is nil when a probe command's failure is its answer (it ran and
// said no, or the manager is not installed), else why it could not tell.
func probeErr(ctx context.Context, command string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", command, ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) || errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("%s: %w", command, err)
}

// Get returns a specific service manager by name.
func Get(name string) (Manager, error) {
	switch name {
	case "systemd":
		return &Systemd{}, nil
	case "launchd":
		return &Launchd{}, nil
	case "nssm":
		return &NSSM{}, nil
	default:
		return nil, fmt.Errorf("unknown service manager: %s", name)
	}
}

// platformManagers returns the service managers to try for the current platform.
func platformManagers() []Manager {
	switch runtime.GOOS {
	case "linux":
		return []Manager{&Systemd{}}
	case "darwin":
		return []Manager{&Launchd{}}
	case "windows":
		return []Manager{&NSSM{}}
	default:
		return nil
	}
}

// SortedKeys returns the keys of a string map in sorted order.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FormatEnvOverride builds a systemd-style override block from env vars.
func FormatEnvOverride(env map[string]string) string {
	var b strings.Builder
	b.WriteString("[Service]\n")
	for _, k := range SortedKeys(env) {
		fmt.Fprintf(&b, "Environment=\"%s=%s\"\n", k, env[k])
	}
	return b.String()
}
