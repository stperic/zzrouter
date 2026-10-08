package install

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/security"
)

// RuntimeCheckContract identifies fixed checks independently of cluster protocol.
const RuntimeCheckContract = "runtime_checks_v1"

// RuntimeCheck reports one independently executed prerequisite check.
type RuntimeCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Actual string `json:"actual"`
	Reason string `json:"reason"`
}

// RuntimeChecksResolver reads this node's current release-owned declarations.
type RuntimeChecksResolver func(provider, runtime string) (schema.RuntimeChecks, error)

//go:embed runtime_probe.py
var runtimeProbe string

// RuntimeCheckCommand renders the fixed predicate used by the verify API.
func RuntimeCheckCommand(python string, checks schema.RuntimeChecks) (string, error) {
	data, err := runtimeCheckData(checks)
	if err != nil {
		return "", err
	}
	return fsroot.ShellQuote(python) + " -I -B -c " + fsroot.ShellQuote(runtimeProbe) + " " + fsroot.ShellQuote(string(data)), nil
}

func runtimeCheckData(checks schema.RuntimeChecks) ([]byte, error) {
	d := schema.Diagnostics{Runtime: "probe", Runtimes: map[string]schema.RuntimeChecks{"probe": checks}}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if len(checks.Checks) == 0 {
		return nil, fmt.Errorf("runtime checks not declared")
	}
	return json.Marshal(checks)
}

// RunRuntimeChecks runs a bounded fixed probe without installing packages.
func RunRuntimeChecks(ctx context.Context, python string, checks schema.RuntimeChecks) []RuntimeCheck {
	return runRuntimeChecks(ctx, python, checks, nil)
}

// RunRuntimeChecksWithEnvironment uses the accepted runtime's explicit launch environment.
func RunRuntimeChecksWithEnvironment(ctx context.Context, python string, checks schema.RuntimeChecks, environment map[string]string, toolkits ...ToolkitSelection) []RuntimeCheck {
	return runRuntimeChecks(ctx, python, checks, environment, toolkits...)
}

func runRuntimeChecks(ctx context.Context, python string, checks schema.RuntimeChecks, environment map[string]string, toolkits ...ToolkitSelection) []RuntimeCheck {
	return runRuntimeChecksSnapshot(ctx, python, checks, environment, nil, toolkits...)
}

// RunRuntimeChecksSnapshot keeps verification on the accepted install environment.
func RunRuntimeChecksSnapshot(ctx context.Context, python string, checks schema.RuntimeChecks, environment map[string]string, executionEnvironment []string, toolkits ...ToolkitSelection) []RuntimeCheck {
	return runRuntimeChecksSnapshot(ctx, python, checks, environment, executionEnvironment, toolkits...)
}

func runRuntimeChecksSnapshot(ctx context.Context, python string, checks schema.RuntimeChecks, environment map[string]string, executionEnvironment []string, toolkits ...ToolkitSelection) []RuntimeCheck {
	data, err := runtimeCheckData(checks)
	if err != nil {
		return failedRuntimeProbe(err)
	}
	line, err := runRuntimeProbeSnapshot(ctx, python, data, environment, "verify", executionEnvironment, toolkits...)
	if line == nil {
		return failedRuntimeProbe(err)
	}
	var result []RuntimeCheck
	if decodeErr := json.Unmarshal(line, &result); decodeErr != nil {
		return failedRuntimeProbe(fmt.Errorf("decode runtime checks: %w", decodeErr))
	}
	if len(result) == 0 {
		return failedRuntimeProbe(fmt.Errorf("runtime probe returned no checks"))
	}
	if err != nil {
		allPassed := true
		for _, check := range result {
			allPassed = allPassed && check.Passed
		}
		if allPassed {
			result = append(result, failedRuntimeProbe(err)...)
		}
	}
	return result
}

func runRuntimeProbe(ctx context.Context, python string, data []byte, environment map[string]string, mode string, toolkits ...ToolkitSelection) ([]byte, error) {
	return runRuntimeProbeSnapshot(ctx, python, data, environment, mode, nil, toolkits...)
}

func runRuntimeProbeSnapshot(ctx context.Context, python string, data []byte, environment map[string]string, mode string, executionEnvironment []string, toolkits ...ToolkitSelection) ([]byte, error) {
	var env []string
	var err error
	if executionEnvironment == nil {
		env, err = process.ChildEnvironment(environment)
	} else {
		env, err = MergeExecutionEnvironment(executionEnvironment, environment)
	}
	if err != nil {
		return nil, err
	}
	for _, toolkit := range toolkits {
		env, err = toolkit.Compose(env)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	scratch, err := os.MkdirTemp("", "zzrouter-probe-")
	if err != nil {
		return nil, err
	}
	// This path is created here, never supplied by API/config.
	defer func() { _ = os.RemoveAll(scratch) }()
	command := host.CommandContext(ctx, python, "-I", "-B", "-c", runtimeProbe, string(data), scratch, mode)
	command.Env = env
	command.WaitDelay = 2 * time.Second
	output := &probeTail{limit: 256 << 10}
	command.Stdout, command.Stderr = output, output
	err = process.RunOwnedCommand(ctx, command)
	raw := output.bytes()
	marker := []byte("ZZROUTER_RUNTIME_CHECKS=")
	index := bytes.LastIndex(raw, marker)
	if index < 0 {
		return nil, fmt.Errorf("runtime probe produced no result (%v): %s", err, security.RedactSensitive(string(raw)))
	}
	line := bytes.SplitN(raw[index+len(marker):], []byte("\n"), 2)[0]
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return security.RedactJSON(line), err
}

func failedRuntimeProbe(err error) []RuntimeCheck {
	reason := security.RedactSensitive(err.Error())
	if len(reason) > 4096 {
		reason = reason[len(reason)-4096:]
	}
	return []RuntimeCheck{{Name: "runtime_probe", Reason: reason}}
}

// probeTail bounds combined subprocess output while continuing to drain it.
type probeTail struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *probeTail) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	if n >= b.limit {
		b.data = append(b.data[:0], data[n-b.limit:]...)
		return n, nil
	}
	if len(b.data)+n > b.limit {
		b.data = b.data[len(b.data)+n-b.limit:]
	}
	b.data = append(b.data, data...)
	return n, nil
}

func (b *probeTail) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data)
}
