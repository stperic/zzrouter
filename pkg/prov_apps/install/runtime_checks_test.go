package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeProbeTailIsBounded(t *testing.T) {
	b := &probeTail{limit: 32}
	for _, data := range [][]byte{bytes.Repeat([]byte("x"), 1000), []byte("final-marker")} {
		n, err := b.Write(data)
		require.NoError(t, err)
		assert.Equal(t, len(data), n)
	}
	assert.Len(t, b.bytes(), 32)
	assert.True(t, bytes.HasSuffix(b.bytes(), []byte("final-marker")))
}

func TestRuntimeChecksCancellationAndRedaction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test executable is a Unix shell fixture")
	}
	checks := schema.RuntimeChecks{Imports: []string{"json"}, Checks: []string{"pip_check", "imports"}, Kernels: "unknown"}
	executable := filepath.Join(t.TempDir(), "python")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\necho 'api_key=super-secret-value' >&2\nexit 1\n"), 0o700))
	result := RunRuntimeChecks(t.Context(), executable, checks)
	require.Len(t, result, 1)
	assert.False(t, result[0].Passed)
	assert.NotContains(t, result[0].Reason, "super-secret-value")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result = RunRuntimeChecks(ctx, executable, checks)
	require.Len(t, result, 1)
	assert.False(t, result[0].Passed)
}

func TestRuntimeChecksStillRunAfterMissingArtifact(t *testing.T) {
	checks := schema.RuntimeChecks{Imports: []string{"json"}, Checks: []string{"pip_check", "imports"}, Kernels: "unknown"}
	plan := Plan{Provider: "arbitrary", Steps: []Step{
		{Number: 1, Verify: StepVerify{Type: "file_exists", Path: filepath.Join(t.TempDir(), "missing")}},
		{Number: 2, Verify: StepVerify{Type: "runtime_checks", Python: filepath.Join(t.TempDir(), "missing-python"), RuntimeChecks: &checks}},
	}}
	result := plan.VerifyAllContext(t.Context())
	assert.False(t, result.AllOK)
	assert.Equal(t, RuntimeCheckContract, result.CheckContract)
	require.Len(t, result.Checks, 1)
	assert.Equal(t, "runtime_probe", result.Checks[0].Name)
	assert.NotContains(t, result.Steps[1].Message, "skipped")
}
