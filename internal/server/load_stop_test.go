package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

type stopFailureModelStore struct{ beforeList func() }

func (s stopFailureModelStore) ListAllModels() ([]*metadata.ModelMetadata, error) {
	if s.beforeList != nil {
		s.beforeList()
	}
	return []*metadata.ModelMetadata{{Name: "model"}}, nil
}

func TestLoadStopsBeforeRelaunchWhenProcessExitIsUnconfirmed(t *testing.T) {
	for _, force := range []bool{true, false} {
		t.Run(map[bool]string{true: "force", false: "failed cleanup"}[force], func(t *testing.T) {
			executor, manager, launches := newAliasLoadExecutor(t)
			run := instance.NewInstance("unreaped", aliasLoadProvider, "model", 8300, time.Minute, 0)
			run.TrackGoroutine()
			t.Cleanup(run.GoroutineDone)
			if force {
				run.SetStatus(instance.StatusRunning)
			} else {
				run.MarkFailed("readiness")
			}
			if force {
				require.NoError(t, manager.Instances().Register(run))
			} else {
				// The run appears after the first lookup, so the later failed-run cleanup must propagate stop failure too.
				executor.registry = stopFailureModelStore{beforeList: func() { require.NoError(t, manager.Instances().Register(run)) }}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			result, err := executor.doLoadLocalModel(ctx, &LoadModelRequest{Provider: aliasLoadProvider, ModelName: "model", Force: force})
			require.ErrorIs(t, err, context.Canceled)
			assert.Nil(t, result)
			assert.Zero(t, *launches)
			retained, ok := manager.Instances().Get(run.ID)
			assert.True(t, ok)
			assert.Same(t, run, retained)
			assert.True(t, run.StopUnconfirmed.Load())
		})
	}
}
