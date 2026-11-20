package server

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStopsBeforeRelaunchWhenProcessExitIsUnconfirmed(t *testing.T) {
	for _, force := range []bool{true, false} {
		t.Run(map[bool]string{true: "force", false: "failed cleanup"}[force], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				executor, manager, launches := newAliasRunsExecutor(t)
				run := instance.NewInstance("unreaped", aliasLoadProvider, "model", 8300, time.Minute, 0)
				run.TrackGoroutine()
				if force {
					run.SetStatus(instance.StatusRunning)
				} else {
					run.MarkFailed("readiness")
				}
				require.NoError(t, manager.Instances().Register(run))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() {
					_, err := startForTest(t, ctx, executor, LaunchRunRequest{Provider: aliasLoadProvider, Model: "model", Replace: force})
					done <- err
				}()
				synctest.Wait()
				require.True(t, run.StopUnconfirmed.Load())
				cancel()
				problem := requireStartProblem(t, <-done, http.StatusInternalServerError, "internal_server_error")
				require.Equal(t, "failed to launch instance: "+context.Canceled.Error(), problem.Detail)
				synctest.Wait()
				assert.Zero(t, *launches)
				retained, ok := manager.Instances().Get(run.ID)
				require.True(t, ok)
				assert.Same(t, run, retained)
				assert.True(t, run.StopUnconfirmed.Load())
				run.GoroutineDone()
				require.NoError(t, manager.Stop(context.Background()))
				synctest.Wait()
			})
		})
	}
}
