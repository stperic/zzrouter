//go:build !windows

package prov_apps

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLifecycleFailure_InlineExitAndDiagnostic(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	inst := instance.NewInstance("failed-engine", "generic-provider", "generic-model", 0, 0, 0)
	inst.LogFilePath = filepath.Join(t.TempDir(), "engine.log")
	file, err := os.Create(inst.LogFilePath)
	require.NoError(t, err)
	require.NoError(t, m.instances.Register(inst))
	probe := &health.ReadinessProbe{Timeout: time.Minute, LogPatterns: health.LogPatterns{Failure: []health.PatternMatcher{{Pattern: "RuntimeError:"}}}}
	require.NoError(t, probe.Validate())
	m.runInstanceLifecycle(inst, "/bin/sh", []string{"-c", "printf 'RuntimeError: incompatible native builds\\n'; exit 23"}, nil, file, health.CheckConfig{ReadinessProbe: probe}, nil)
	snapshot := inst.ToInfo("worker")
	require.Equal(t, instance.StatusFailed, snapshot.Status)
	require.NotNil(t, snapshot.Failure)
	require.NotNil(t, snapshot.Failure.ExitCode)
	assert.Equal(t, 23, *snapshot.Failure.ExitCode)
	assert.Equal(t, "RuntimeError: incompatible native builds", snapshot.ErrorMessage)
	assert.Contains(t, snapshot.Failure.ErrorTail, "RuntimeError: incompatible native builds")
	inst.Cancel()
}

func TestDeclaredReadinessAndExitFailures(t *testing.T) {
	for _, tc := range []struct{ provider, line string }{
		{"vllm", "OutOfMemoryError: CUDA out of memory. Tried to allocate 24.50 GiB. GPU 0 has a total capacity of 94.97 GiB of which 17.18 GiB is free."},
		{"vllm", "No available memory for the cache blocks"},
		{"llamacpp", "error: cudaMalloc failed: out of memory"},
		{"llamacpp", "error: failed to allocate CUDA0 buffer"},
		{"mlx", "RuntimeError: [malloc] Unable to allocate 1024 bytes."},
	} {
		t.Run(tc.provider+tc.line, func(t *testing.T) {
			raw, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + tc.provider + "/config.yaml")
			require.NoError(t, err)
			var cfg config.ServiceConfig
			require.NoError(t, yaml.Unmarshal(raw, &cfg))
			require.NoError(t, cfg.Runtime.FailurePatterns.Compile())
			probe := buildHealthCheckConfig(cfg, 0).ReadinessProbe
			require.NotNil(t, probe)
			probe.LogPatterns.Success = []health.PatternMatcher{{Pattern: "ready"}}
			for _, generic := range []string{"error:", "RuntimeError:", ""} {
				probe.LogPatterns.Failure = []health.PatternMatcher{{Pattern: generic}}
				writer := health.NewReadinessWriter(io.Discard, probe)
				_, err = writer.Write([]byte(tc.line + "\n"))
				require.NoError(t, err)
				inst := instance.NewInstance("failed", tc.provider, "model", 0, 0, 0)
				manager := &ProviderAppManager{}
				manager.watchReadiness(inst, writer, probe)
				var cause *httperr.EngineFailure
				require.ErrorAs(t, inst.FailureCause(), &cause)
				assert.Equal(t, httperr.FailureMemory, cause.Class)
			}
			for _, status := range []instance.Status{instance.StatusStarting, instance.StatusRunning} {
				inst := instance.NewInstance("exit", tc.provider, "model", 0, 0, 0)
				inst.FailureClassifier = &cfg.Runtime.FailurePatterns
				inst.SetStatus(status)
				inst.LogFilePath = filepath.Join(t.TempDir(), "run.log")
				require.NoError(t, os.WriteFile(inst.LogFilePath, []byte("Traceback (most recent call last)\n"+tc.line+"\n"), 0600))
				code := 1
				inst.MarkFailedWithExit("process exited", &code, "")
				var cause *httperr.EngineFailure
				require.ErrorAs(t, inst.FailureCause(), &cause)
				assert.Equal(t, httperr.FailureMemory, cause.Class)
				assert.Equal(t, code, *inst.ToInfo("").Failure.ExitCode)
			}
		})
	}
}
