package process

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestLauncherAppliesPreparedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix pwd fixture")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	run := instance.NewInstance("working-directory", "studio", "", 0, 0, 0)
	defer run.Cancel()
	run.SetResolved(instance.Resolved{WorkingDir: dir})
	var output bytes.Buffer
	result, err := NewLauncher(&SecurityConfig{}, true, t.TempDir()).Launch(t.Context(), run, "/bin/pwd", nil, nil, &output)
	require.NoError(t, err)
	_, err = result.Wait()
	require.NoError(t, err)
	if result.StdinPipe != nil {
		_ = result.StdinPipe.Close()
	}
	require.Equal(t, dir, strings.TrimSpace(output.String()))
	require.Equal(t, dir, result.Command.WorkingDir)
	svc := config.ServiceConfig{Name: "studio", Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "python", WorkingDir: "source"}}}
	prepared, err := NewCommandBuilder(&svc).Build(ResolvedModel{}, 8188, nil)
	require.NoError(t, err)
	require.Equal(t, fsroot.ProviderDir("studio")+"/source", prepared.WorkingDir)
}
