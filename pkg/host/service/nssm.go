package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
)

// NSSM manages services via NSSM (Non-Sucking Service Manager) on Windows.
// NSSM wraps executables as Windows services with full env var support.
//
// Install: https://nssm.cc/ or `winget install nssm`
type NSSM struct{}

func (n *NSSM) Name() string { return "nssm" }

func (n *NSSM) Detect(serviceName string) bool {
	return n.DetectContext(context.Background(), serviceName)
}

func (n *NSSM) DetectContext(ctx context.Context, serviceName string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := host.CommandContext(ctx, "nssm", "status", serviceName).Output()
	if err != nil {
		return false
	}
	output := strings.TrimSpace(string(out))
	return output != "" && !strings.Contains(output, "Can't open service")
}

func (n *NSSM) ApplyEnv(serviceName string, env map[string]string) *ApplyResult {
	return n.ApplyEnvContext(context.Background(), serviceName, env)
}

// ApplyEnvContext bounds environment application and restart by the caller.
func (n *NSSM) ApplyEnvContext(ctx context.Context, serviceName string, env map[string]string) *ApplyResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result := &ApplyResult{Manager: "nssm", EnvVarsCount: len(env)}
	if err := ctx.Err(); err != nil {
		result.Error = err.Error()
		return result
	}

	var pairs []string
	for _, k := range SortedKeys(env) {
		pairs = append(pairs, k+"="+env[k])
	}
	envStr := strings.Join(pairs, "\n")

	if out, err := host.CommandContext(ctx, "nssm", "set", serviceName, "AppEnvironmentExtra", envStr).CombinedOutput(); err != nil {
		result.Error = fmt.Sprintf("nssm set failed: %s", strings.TrimSpace(string(out)))
		result.Instructions = n.Instructions(serviceName, env)
		return result
	}

	if out, err := host.CommandContext(ctx, "nssm", "restart", serviceName).CombinedOutput(); err != nil {
		result.Applied = true
		result.Error = fmt.Sprintf("nssm restart failed: %s", strings.TrimSpace(string(out)))
		result.Instructions = fmt.Sprintf("nssm restart %s", serviceName)
		return result
	}

	result.Applied = true
	result.Restarted = true
	return result
}

func (n *NSSM) Restart(serviceName string) error {
	if out, err := host.Command("nssm", "restart", serviceName).CombinedOutput(); err != nil {
		return fmt.Errorf("nssm restart %s failed: %s", serviceName, strings.TrimSpace(string(out)))
	}
	return nil
}

func (n *NSSM) Status(serviceName string) ServiceStatus {
	return n.StatusContext(context.Background(), serviceName)
}

func (n *NSSM) StatusContext(ctx context.Context, serviceName string) ServiceStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status := ServiceStatus{Manager: "nssm"}

	out, err := host.CommandContext(ctx, "nssm", "status", serviceName).Output()
	if err != nil {
		status.ObservationError = fmt.Errorf("nssm status: %w", err)
		return status
	}

	output := strings.TrimSpace(string(out))
	switch output {
	case "SERVICE_RUNNING":
		status.Running = true
	case "SERVICE_STOPPED":
		status.Running = false
	default:
		status.ObservationError = fmt.Errorf("nssm state is %q; running state is not established", output)
	}

	return status
}

func (n *NSSM) Instructions(serviceName string, env map[string]string) string {
	var b strings.Builder
	b.WriteString("# Run as Administrator:\n")

	first := true
	for _, k := range SortedKeys(env) {
		prefix := "+"
		if first {
			prefix = ""
			first = false
		}
		fmt.Fprintf(&b, "nssm set %s AppEnvironmentExtra %s%s=%s\n", serviceName, prefix, k, env[k])
	}
	fmt.Fprintf(&b, "nssm restart %s\n", serviceName)

	return b.String()
}
