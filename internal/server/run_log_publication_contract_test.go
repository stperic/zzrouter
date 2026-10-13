package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueuedRunLogsContract(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte("#!/bin/sh\necho 'GPU-A, 0, 10000, 6000'\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	r := newReadinessRig(t, prov_apps.WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	}))
	svc, ok := r.mgr.AppsConfig().LookupApp("fake")
	require.True(t, ok)
	svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55"}}
	fresh := &config.AppsConfig{}
	require.NoError(t, fresh.AddApp("fake", svc))
	r.mgr.ReloadConfig(fresh)
	first, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "first"})
	require.NoError(t, err)
	require.True(t, first.Run.Admitted())
	second, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "queued"})
	require.NoError(t, err)
	queued := second.Run
	require.False(t, queued.Admitted())
	assert.Zero(t, queued.GetProcessID())
	assert.Equal(t, instance.StatusStarting, queued.GetStatus())
	l := &LogsHandlers{
		appMgr: r.mgr, getClusterClient: func() mesh.ClusterClient { return nil },
		isLocalNode: func(string) bool { return true },
	}
	for _, internal := range []bool{false, true} {
		name, prefix := "public", "/zzrouter/v1/runs/"
		if internal {
			name, prefix = "internal", "/zzrouter/v1/internal/runs/"
		}
		t.Run(name, func(t *testing.T) {
			c, rec := testGinContext(http.MethodGet, prefix+queued.ID+"/logs")
			c.Params = gin.Params{{Key: "id", Value: queued.ID}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			if internal {
				(&RunsExecutor{appMgr: r.mgr, getInstanceLogLines: l.getInstanceLogLines}).HandleInternalGetRunLogs(c)
			} else {
				l.HandleGetRunLogsPublic(c)
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"success":true`)
		})
	}
	require.False(t, queued.Admitted(), "reading logs does not admit or launch the queued run")
	require.NoError(t, r.mgr.StopInstance(t.Context(), queued.ID))
	require.FileExists(t, filepath.Join(r.mgr.LogManager().GetBaseDir(), queued.ID+".log"))
	c, rec := testGinContext(http.MethodGet, "/zzrouter/v1/runs/"+queued.ID+"/logs")
	c.Params = gin.Params{{Key: "id", Value: queued.ID}}
	l.HandleGetRunLogsPublic(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "stopped")
}
