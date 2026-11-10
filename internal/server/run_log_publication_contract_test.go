package server

import (
	"context"
	"net/http"
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
	r := newReadinessRig(t, prov_apps.WithProviderSchema(func(string) (*schema.ProviderSchema, error) {
		return &schema.ProviderSchema{Parameters: map[string]schema.ParamShape{"budget": {}}, Diagnostics: &schema.Diagnostics{Memory: &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}}}, nil
	}))
	svc, ok := r.mgr.AppsConfig().LookupApp("fake")
	require.True(t, ok)
	svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55"}}
	fresh := &config.AppsConfig{}
	require.NoError(t, fresh.AddApp("fake", svc))
	r.mgr.ReloadConfig(fresh)
	first := instance.NewInstance("starting-blocker", "fake", "first", 0, 0, 0)
	require.True(t, first.Admit())
	require.NoError(t, r.mgr.Instances().Register(first))
	second, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "queued"})
	require.NoError(t, err)
	queued := second.Run
	require.False(t, queued.Admitted())
	assert.Equal(t, first.ID, queued.WaitingOn())
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
