package prov_apps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queuedLogRun parks admission without starting a fixture process.
func queuedLogRun(t *testing.T) (queueFixture, *instance.Instance, func()) {
	t.Helper()
	f := newQueueFixture(t)
	f.mgr.memoryAdmission <- struct{}{}
	release := sync.OnceFunc(func() { <-f.mgr.memoryAdmission })
	run := f.launch(t, "queued-log")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if f.mgr.instances.Has(run.ID) {
			assert.NoError(t, f.mgr.StopInstance(ctx, run.ID))
		}
		release()
	})
	return f, run, release
}

func TestRunLogPublicationContract(t *testing.T) {
	t.Run("queued log exists before admission", func(t *testing.T) {
		f, run, _ := queuedLogRun(t)
		published, ok := f.mgr.GetInstance(run.ID)
		require.True(t, ok)
		require.Same(t, run, published)
		require.NotEmpty(t, published.LogFilePath, "a published queued run already owns its log")
		require.FileExists(t, published.LogFilePath)
		assert.False(t, run.Admitted())
		assert.Equal(t, instance.StatusStarting, run.GetStatus())
		assert.Zero(t, run.GetProcessID())
		select {
		case <-run.LeftStarting():
			t.Fatal("creating a queued log must not publish readiness")
		default:
		}
		assert.NoFileExists(t, f.engine+".started")
	})

	t.Run("published path is immutable through concurrent admission", func(t *testing.T) {
		f, run, release := queuedLogRun(t)
		path := run.LogFilePath
		stop, started, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		stopReader := sync.OnceFunc(func() { close(stop); <-finished })
		changed := make(chan string, 1)
		go func() {
			defer close(finished)
			close(started)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if published, ok := f.mgr.GetInstance(run.ID); ok && published.LogFilePath != path {
					select {
					case changed <- published.LogFilePath:
					default:
					}
				}
				runtime.Gosched()
			}
		}()
		t.Cleanup(stopReader)
		<-started
		release()
		f.release(t, run)
		stopReader()
		select {
		case got := <-changed:
			t.Fatalf("published path changed from %q to %q", path, got)
		default:
		}
		require.NotEmpty(t, path)
		assert.Equal(t, path, run.LogFilePath)
		assert.Contains(t, runLog(t, run), "READY")
	})

	t.Run("cancelled queue retains log and releases port", func(t *testing.T) {
		f, run, _ := queuedLogRun(t)
		path := run.LogFilePath
		require.NotEmpty(t, path)
		require.NoError(t, os.WriteFile(path, []byte("queued diagnostic\n"), 0o600))
		require.NoError(t, f.mgr.StopInstance(t.Context(), run.ID))
		assert.False(t, f.mgr.instances.Has(run.ID))
		assert.Zero(t, run.GetProcessID())
		assert.NoFileExists(t, f.engine+".started")
		assert.Contains(t, runLog(t, run), "queued diagnostic")
		assert.Contains(t, runLog(t, run), "stopped")
		port, err := f.mgr.ports.AllocateForProvider("generic", run.Port)
		require.NoError(t, err)
		assert.Equal(t, run.Port, port)
		f.mgr.ports.ReleaseForProvider("generic", port)
	})

	t.Run("refused queue keeps its log cause and port until stop", func(t *testing.T) {
		f := newQueueFixture(t)
		first, queued := f.launch(t, "first"), f.launch(t, "refused")
		path := queued.LogFilePath
		require.NotEmpty(t, path)
		require.NoError(t, os.WriteFile(path, []byte("waiting for memory\n"), 0o600))
		f.setFree("4000")
		f.release(t, first)
		eventually(t, func() bool { return queued.GetStatus() == instance.StatusFailed }, "queued admission refused")
		var fit *process.MemoryFitError
		require.ErrorAs(t, queued.FailureCause(), &fit)
		assert.True(t, f.mgr.instances.Has(queued.ID))
		assert.Zero(t, queued.GetProcessID())
		assert.Equal(t, path, queued.LogFilePath)
		assert.Contains(t, runLog(t, queued), "waiting for memory")
		assert.True(t, f.mgr.ports.IsAllocatedForProvider("generic", queued.Port), "a listed failed run retains its port")
		require.NoError(t, f.mgr.StopInstance(t.Context(), queued.ID))
		assert.False(t, f.mgr.ports.IsAllocatedForProvider("generic", queued.Port))
		port, err := f.mgr.ports.AllocateForProvider("generic", queued.Port)
		require.NoError(t, err)
		assert.Equal(t, queued.Port, port)
		f.mgr.ports.ReleaseForProvider("generic", port)
		assert.Contains(t, runLog(t, queued), "waiting for memory")
	})

	t.Run("log creation failure never publishes and releases port", func(t *testing.T) {
		t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
		blocked := config.Paths().GetLogsDir()
		require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0o600))
		f := newQueueFixture(t)
		requested, err := f.mgr.ports.AllocateForProvider("generic", 8630)
		require.NoError(t, err)
		f.mgr.ports.ReleaseForProvider("generic", requested)
		_, err = f.mgr.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "blocked", Port: requested})
		require.Error(t, err)
		assert.Empty(t, f.mgr.instances.List())
		select {
		case <-f.mgr.instances.Changes():
			t.Fatal("log creation failed after registry publication")
		default:
		}
		assert.NoFileExists(t, f.engine+".started")
		assert.Zero(t, f.mgr.ports.GetPoolStats()["generic"].AllocatedPorts)
		port, err := f.mgr.ports.AllocateForProvider("generic", requested)
		require.NoError(t, err)
		assert.Equal(t, requested, port)
		f.mgr.ports.ReleaseForProvider("generic", port)
	})

	for _, missing := range []bool{false, true} {
		name := "moved daemon precedes log failure"
		if missing {
			name = "missing daemon precedes log failure"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			require.NoError(t, os.WriteFile(config.Paths().GetLogsDir(), []byte("not a directory"), 0o600))
			r := newServedRig(t)
			svc, ok := r.m.AppsConfig().LookupApp("ollama")
			require.True(t, ok)
			if missing {
				fresh := &config.AppsConfig{}
				r.config.Store(fresh)
				r.m.ReloadConfig(fresh)
			}
			var observed []string
			if !missing {
				observed = []string{"http://previous-daemon:11434"}
			}
			run, _, err := (servedEngine{m: r.m}).servedRun(t.Context(), EnsureRequest{Provider: "ollama", Model: servedModel}, svc, "ollama", observed...)
			require.ErrorIs(t, err, ErrProviderUnavailable)
			assert.Nil(t, run)
			assert.Empty(t, r.m.instances.List())
		})
	}
}

// Admission must reopen the queued log rather than discard its earlier diagnostics.
func TestRunLogAdmissionContractPreservesQueuedHistory(t *testing.T) {
	f, run, release := queuedLogRun(t)
	path := run.LogFilePath
	require.NotEmpty(t, path)
	require.NoError(t, os.WriteFile(path, []byte("before admission\n"), 0o600))
	release()
	f.release(t, run)
	assert.Equal(t, path, run.LogFilePath)
	assert.Contains(t, runLog(t, run), "before admission")
	require.NoError(t, f.mgr.StopInstance(t.Context(), run.ID))
	body, err := os.ReadFile(filepath.Join(f.mgr.LogManager().GetBaseDir(), run.ID+".log"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "before admission")
}
