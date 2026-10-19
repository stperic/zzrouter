//go:build unix

package prov_apps

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/require"
)

func TestWrappedStopEngineHelper(t *testing.T) {
	if !slices.Contains(os.Args, "wrapped-stop-engine") {
		return
	}
	dir := os.Args[len(os.Args)-1]
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); err != nil {
		os.Exit(2)
	}
	var finished <-chan time.Time
	count := 0
	for {
		select {
		case <-signals:
			count++
			if count == 1 {
				finished = time.After(6 * time.Second) // Outlive the launcher's standalone escalation window.
			}
		case <-finished:
			if err := os.WriteFile(filepath.Join(dir, "signals"), []byte{byte(count)}, 0600); err != nil {
				os.Exit(2)
			}
			return
		}
	}
}

func TestTerminateProcessWrappedReceivesOneSignal(t *testing.T) {
	dir := t.TempDir()
	wrapper := os.Getenv("ZZROUTER_TEST_LAUNCHER")
	if wrapper == "" {
		wrapper = filepath.Join(dir, "zzrouter-launcher")
		output, err := host.Command("go", "build", "-o", wrapper, "../../cmd/zzrouter-launcher").CombinedOutput()
		require.NoError(t, err, "%s", output)
	} else {
		body, err := os.ReadFile(wrapper)
		require.NoError(t, err)
		wrapper = filepath.Join(dir, "zzrouter-launcher")
		require.NoError(t, os.WriteFile(wrapper, body, 0700))
	}
	t.Setenv("PATH", dir)
	binary, err := os.Executable()
	require.NoError(t, err)
	inst := instance.NewInstance("one-signal", "fixture", "model", 0, 0, 8)
	result, err := process.NewLauncher(&process.SecurityConfig{}, true, dir).Launch(t.Context(), inst, binary,
		[]string{"-test.run=^TestWrappedStopEngineHelper$", "--", "wrapped-stop-engine", dir}, nil, nil)
	require.NoError(t, err)
	inst.SetProcessInfo(result.PID, result.ProcessGroupID, result.StartToken, result.Command)
	inst.SetStdinPipe(result.StdinPipe)
	done := make(chan error, 1)
	go func() { _, err := result.Wait(); done <- err; close(done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-result.ProcessGroupID, syscall.SIGKILL)
		inst.CloseStdinPipe()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("wrapped engine was not reaped")
		}
	})
	require.True(t, inst.IsUsingLauncher())
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("fixture", config.ServiceConfig{Mode: "on-demand", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{BasePort: 8100, Execution: config.ExecutionConfig{Type: "cli", Command: binary}},
		Defaults:     &config.AppDefaultsConfig{Metadata: &config.AppMetadata{ShutdownTimeout: "8s"}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	manager, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	require.NoError(t, manager.terminateProcess(t.Context(), inst, result.PID, result.StartToken))
	require.NoError(t, <-done)
	count, err := os.ReadFile(filepath.Join(dir, "signals"))
	require.NoError(t, err, "the engine must finish its graceful shutdown beyond five seconds")
	require.Equal(t, []byte{1}, count, "a wrapped engine must receive exactly one shutdown signal")
}
