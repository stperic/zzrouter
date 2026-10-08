//go:build !windows

package process

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchComposesTrustedEnvironmentAfterValidation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	launcher := NewLauncher(nil, true, "")
	called := false
	transform := func(base []string) ([]string, error) {
		called = true
		return append(base, "LD_LIBRARY_PATH=/trusted/managed/lib"), nil
	}
	for _, key := range []string{"PATH", "LD_LIBRARY_PATH"} {
		_, err := launcher.Launch(t.Context(), &instance.Instance{}, "/bin/sh", nil, map[string]string{key: "/caller"}, nil, transform)
		require.ErrorIs(t, err, ErrDangerousEnvVar)
		assert.False(t, called)
	}
	var output bytes.Buffer
	inst := &instance.Instance{ID: "trusted-environment", Provider: "test", Logs: make(chan string, 8)}
	result, err := launcher.Launch(t.Context(), inst, "/bin/sh", []string{"-c", "printf '%s' \"$LD_LIBRARY_PATH\""}, nil, &output, transform)
	require.NoError(t, err)
	_, err = result.Wait()
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "/trusted/managed/lib", output.String())
	failure := errors.New("invalid managed selection")
	_, err = launcher.Launch(t.Context(), inst, "/not-executed", nil, nil, nil, func([]string) ([]string, error) { return nil, failure })
	assert.ErrorIs(t, err, failure)
}
