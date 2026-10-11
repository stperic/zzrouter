package server

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadStopsBeforeRelaunchWhenProcessExitIsUnconfirmed(t *testing.T) {
	for _, force := range []bool{true, false} {
		t.Run(map[bool]string{true: "force", false: "failed cleanup"}[force], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				executor, manager, launches := newAliasLoadExecutor(t)
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
					_, err := executor.LoadLocalModel(ctx, &LoadModelRequest{Provider: aliasLoadProvider, ModelName: "model", Force: force})
					done <- err
				}()
				synctest.Wait()
				require.True(t, run.StopUnconfirmed.Load())
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
				synctest.Wait()
				assert.Zero(t, *launches)
				retained, ok := manager.Instances().Get(run.ID)
				require.True(t, ok)
				assert.Same(t, run, retained)
				assert.True(t, run.StopUnconfirmed.Load())
				run.GoroutineDone()
				synctest.Wait()
			})
		})
	}
}
