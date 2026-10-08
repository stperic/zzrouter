//go:build !windows

package prov_apps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
