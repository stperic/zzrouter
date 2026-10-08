package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
)

// Systemd manages services via systemd (Linux).
//
// Env vars are persisted in a systemd override file at:
//
//	/etc/systemd/system/<service>.service.d/override.conf
//
// This is the official Ollama-documented method (`sudo systemctl edit ollama`).
// Survives service upgrades because overrides are in a separate .d/ directory.
//
// Note: writing the override requires root. If zzRouter doesn't have write access,
// ApplyEnv returns the manual instructions instead.
type Systemd struct{}

func (s *Systemd) Name() string { return "systemd" }

func (s *Systemd) Detect(serviceName string) bool {
	return s.DetectContext(context.Background(), serviceName)
}

func (s *Systemd) DetectContext(ctx context.Context, serviceName string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := host.CommandContext(ctx, "systemctl", "show", serviceName, "--property=LoadState").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "LoadState=loaded")
}

func (s *Systemd) ApplyEnv(serviceName string, env map[string]string) *ApplyResult {
	return s.ApplyEnvContext(context.Background(), serviceName, env)
}

// ApplyEnvContext bounds environment application and restart by the caller.
func (s *Systemd) ApplyEnvContext(ctx context.Context, serviceName string, env map[string]string) *ApplyResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result := &ApplyResult{Manager: "systemd", EnvVarsCount: len(env)}
	if err := ctx.Err(); err != nil {
		result.Error = err.Error()
		return result
	}

	overrideDir := filepath.Dir(s.overridePath(serviceName))
	overridePath := s.overridePath(serviceName)
	content := FormatEnvOverride(env)

	// Try to create override directory
	if err := os.MkdirAll(overrideDir, 0755); err != nil {
		result.Error = "permission denied: cannot create systemd override directory"
		result.Instructions = s.Instructions(serviceName, env)
		return result
	}

	// Try to write override file
	if err := os.WriteFile(overridePath, []byte(content), 0644); err != nil {
		result.Error = "permission denied: cannot write systemd override file"
		result.Instructions = s.Instructions(serviceName, env)
		return result
	}

	// Daemon-reload
	if out, err := host.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		result.Error = fmt.Sprintf("systemctl daemon-reload failed: %s", strings.TrimSpace(string(out)))
		result.Instructions = s.Instructions(serviceName, env)
		return result
	}

	// Restart
	if out, err := host.CommandContext(ctx, "systemctl", "restart", serviceName).CombinedOutput(); err != nil {
		result.Applied = true // File was written, just restart failed
		result.Error = fmt.Sprintf("systemctl restart failed: %s", strings.TrimSpace(string(out)))
		result.Instructions = fmt.Sprintf("sudo systemctl restart %s", serviceName)
		return result
	}

	result.Applied = true
	result.Restarted = true
	return result
}

func (s *Systemd) Restart(serviceName string) error {
	out, err := host.Command("systemctl", "restart", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s failed: %s", serviceName, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Systemd) Status(serviceName string) ServiceStatus {
	return s.StatusContext(context.Background(), serviceName)
}

func (s *Systemd) StatusContext(ctx context.Context, serviceName string) ServiceStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status := ServiceStatus{Manager: "systemd"}

	out, err := host.CommandContext(ctx, "systemctl", "show", serviceName,
		"--property=ActiveState,MainPID").Output()
	if err != nil {
		status.ObservationError = fmt.Errorf("systemctl show: %w", err)
		return status
	}

	observed := false
	for line := range strings.SplitSeq(string(out), "\n") {
		if after, ok := strings.CutPrefix(line, "ActiveState="); ok {
			observed = true
			switch after {
			case "active", "reloading":
				status.Running = true
			case "inactive", "failed":
				status.Running = false
			default:
				status.ObservationError = fmt.Errorf("systemd state is %q; running state is not established", after)
			}
		}
		if after, ok := strings.CutPrefix(line, "MainPID="); ok {
			pid, _ := strconv.Atoi(after)
			status.PID = pid
		}
	}
	if !observed {
		status.ObservationError = fmt.Errorf("systemd response omitted ActiveState")
	}

	return status
}

func (s *Systemd) Instructions(serviceName string, env map[string]string) string {
	content := FormatEnvOverride(env)
	return fmt.Sprintf("sudo systemctl edit %s.service\n\nPaste this content:\n\n%s\nThen run:\nsudo systemctl daemon-reload\nsudo systemctl restart %s",
		serviceName, content, serviceName)
}

func (s *Systemd) overridePath(serviceName string) string {
	return fmt.Sprintf("/etc/systemd/system/%s.service.d/override.conf", serviceName)
}
