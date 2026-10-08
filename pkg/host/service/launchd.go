package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
)

// Launchd manages services via launchd (macOS).
//
// Supports Homebrew (`brew services`), custom plists, and Ollama.app.
// Uses `launchctl setenv` for immediate env var changes and `brew services restart`
// or `launchctl stop` for restarts.
//
// Note: `launchctl setenv` does NOT persist across reboots. This is a known
// macOS limitation documented by Ollama. For persistence, users must modify
// their launch plist's EnvironmentVariables dict.
type Launchd struct {
	detectedLabel string
	detectedPlist string
	isBrew        bool
}

func (l *Launchd) Name() string { return "launchd" }

func (l *Launchd) Detect(serviceName string) bool {
	return l.DetectContext(context.Background(), serviceName)
}

func (l *Launchd) DetectContext(ctx context.Context, serviceName string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Try known labels directly (O(1) per label) instead of listing all services
	candidates := []string{
		"homebrew.mxcl." + serviceName,
		"com." + serviceName + ".serve",
		"com." + serviceName,
	}
	for _, label := range candidates {
		if err := host.CommandContext(ctx, "launchctl", "list", label).Run(); err == nil {
			l.detectedLabel = label
			l.isBrew = strings.HasPrefix(label, "homebrew.mxcl.")
			l.detectedPlist = l.findPlistForLabel(label)
			return true
		}
	}
	return false
}

func (l *Launchd) ApplyEnv(serviceName string, env map[string]string) *ApplyResult {
	return l.ApplyEnvContext(context.Background(), serviceName, env)
}

// ApplyEnvContext bounds environment application and restart by the caller.
func (l *Launchd) ApplyEnvContext(ctx context.Context, serviceName string, env map[string]string) *ApplyResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result := &ApplyResult{Manager: "launchd", EnvVarsCount: len(env)}
	if err := ctx.Err(); err != nil {
		result.Error = err.Error()
		return result
	}

	// Apply env vars via launchctl setenv (session-level, immediate)
	for k, v := range env {
		if out, err := host.CommandContext(ctx, "launchctl", "setenv", k, v).CombinedOutput(); err != nil {
			result.Error = fmt.Sprintf("launchctl setenv %s failed: %s", k, strings.TrimSpace(string(out)))
			result.Instructions = l.Instructions(serviceName, env)
			return result
		}
	}

	// Restart
	if err := l.Control(ctx, serviceName, "restart"); err != nil {
		result.Applied = true // env vars set, restart failed
		result.Error = err.Error()
		result.Instructions = l.restartInstructions(serviceName)
		return result
	}

	result.Applied = true
	result.Restarted = true
	result.Warning = "launchctl setenv does not persist across reboots (macOS limitation)"
	return result
}

func (l *Launchd) Restart(serviceName string) error {
	if l.isBrew {
		if out, err := host.Command("brew", "services", "restart", serviceName).CombinedOutput(); err != nil {
			return fmt.Errorf("brew services restart %s failed: %s", serviceName, strings.TrimSpace(string(out)))
		}
		return nil
	}

	if l.detectedPlist != "" {
		_ = host.Command("launchctl", "unload", l.detectedPlist).Run()
		if out, err := host.Command("launchctl", "load", l.detectedPlist).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl load failed: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}

	if l.detectedLabel != "" {
		_ = host.Command("launchctl", "stop", l.detectedLabel).Run()
		return nil
	}

	return fmt.Errorf("no plist or label found to restart %s", serviceName)
}

func (l *Launchd) Status(serviceName string) ServiceStatus {
	return l.StatusContext(context.Background(), serviceName)
}

func (l *Launchd) StatusContext(ctx context.Context, serviceName string) ServiceStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status := ServiceStatus{Manager: "launchd"}

	if l.detectedLabel == "" {
		status.ObservationError = fmt.Errorf("launchd service label is unavailable")
		return status
	}

	// Check PID via pgrep
	if out, err := host.CommandContext(ctx, "pgrep", "-x", serviceName).Output(); err == nil {
		pidStr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
		if pid, err := strconv.Atoi(pidStr); err == nil {
			status.Running = true
			status.PID = pid
		} else {
			status.ObservationError = fmt.Errorf("pgrep returned an invalid provider PID: %w", err)
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			status.ObservationError = fmt.Errorf("pgrep provider: %w", err)
		}
	}

	return status
}

func (l *Launchd) Instructions(serviceName string, env map[string]string) string {
	var b strings.Builder

	// Immediate (session-level)
	b.WriteString("# Apply immediately (current session only):\n")
	keys := SortedKeys(env)
	for _, k := range keys {
		fmt.Fprintf(&b, "launchctl setenv %s \"%s\"\n", k, env[k])
	}
	b.WriteString(l.restartInstructions(serviceName))

	// Persistence note
	b.WriteString("\n# Note: launchctl setenv does NOT persist across reboots.\n")
	b.WriteString("# For persistence, add EnvironmentVariables to your Ollama plist.\n")

	return b.String()
}

func (l *Launchd) restartInstructions(serviceName string) string {
	if l.isBrew {
		return fmt.Sprintf("\n# Restart:\nbrew services restart %s\n", serviceName)
	}
	return fmt.Sprintf("\n# Restart:\nlaunchctl stop %s\n", l.detectedLabel)
}

func (l *Launchd) findPlistForLabel(label string) string {
	home, _ := os.UserHomeDir()
	paths := []string{
		filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		filepath.Join("/Library/LaunchAgents", label+".plist"),
		filepath.Join("/Library/LaunchDaemons", label+".plist"),
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
