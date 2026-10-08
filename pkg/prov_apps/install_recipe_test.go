package prov_apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptedRecipeFinalizationFailureRollsBack(t *testing.T) {
	for _, failure := range []string{"finalize", "completion", "panic", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			isolateProviderRoot(t)
			coord, _, reg, cleanup := newAsyncTestCoord(t, "vllm")
			defer cleanup()
			var rolledBack atomic.Bool
			reached := make(chan struct{})
			release := make(chan struct{})
			plan := &install.Plan{Provider: "vllm", Action: "install", Version: "1", PlanID: "accepted", Recipe: &install.RecipeSnapshot{}, Rollback: func() error { rolledBack.Store(true); return nil }}
			plan.Steps = []install.Step{{Number: 1, PostExec: func(ctx context.Context) error {
				close(reached)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}}}
			if failure == "completion" {
				plan.Steps[0].Verify = install.StepVerify{Type: "file_exists", Path: "/a-file-that-does-not-exist"}
			}
			finalize := func() error {
				if failure == "panic" {
					panic("finalize panic")
				}
				return errors.New("save rejected")
			}
			requestCtx, requestCancel := context.WithCancel(t.Context())
			id, err := coord.ExecuteResolvedAsync(requestCtx, "vllm", plan, install.PlanOptions{Force: true}, -1, finalize)
			require.NoError(t, err)
			requestCancel()
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("job inherited HTTP cancellation")
			}
			sub, err := reg.Subscribe(id, jobs.SubscribeOptions{})
			require.NoError(t, err)
			defer sub.Unsubscribe()
			if failure == "cancellation" {
				require.NoError(t, reg.Cancel(id))
			} else {
				close(release)
			}
			terminal := waitTerminal(t, sub.Events(), 2*time.Second)
			assert.Equal(t, jobs.PhaseFailed, terminal.Phase)
			assert.True(t, rolledBack.Load(), "failure escaped rollback")
		})
	}
}

type resolvedPlanInstaller struct {
	*fakeInstaller
	plan install.Plan
}

func (i *resolvedPlanInstaller) InstallPlan(context.Context, string) (*install.Plan, error) {
	copy := i.plan
	return &copy, nil
}

func TestGuidedIntermediateRecipeStepDoesNotFinalizeHealthyLiveRuntime(t *testing.T) {
	isolateProviderRoot(t)
	coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()
	builder := &resolvedPlanInstaller{fakeInstaller: fake, plan: install.Plan{Provider: "vllm", Action: "install", Version: "1", PlanID: "raw", Recipe: &install.RecipeSnapshot{}, Steps: []install.Step{{Number: 1}, {Number: 2, Verify: install.StepVerify{Type: "file_exists", Path: "/missing-final-runtime"}}}}}
	coord.dispatcher.Register("vllm", builder)
	opts := install.PlanOptions{Force: true}
	plan, err := coord.PrepareInstallPlan(t.Context(), "vllm", "1", opts)
	require.NoError(t, err)
	opts.ExpectedPlanID = plan.PlanID
	h := coord.openJob("install", "vllm")
	require.NotNil(t, h)
	var finalized bool
	err = coord.runResolvedPlan(h, "vllm", plan, opts, 1, func() error { finalized = true; return nil })
	require.NoError(t, err, "an intermediate step must not verify the final installation")
	assert.False(t, finalized)
	h.Done()
}

func TestTypedUpgradeRequiresExistingManagedRuntime(t *testing.T) {
	isolateProviderRoot(t)
	coord, _, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()
	plan := &install.Plan{Provider: "vllm", Action: "upgrade", Recipe: &install.RecipeSnapshot{}}
	_, err := coord.ExecuteResolvedAsync(t.Context(), "vllm", plan, install.PlanOptions{Action: "upgrade"}, -1, nil)
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

func TestPanicRollbackReportsPreviousRuntimeAndRestoration(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for _, rollbackFails := range []bool{false, true} {
			name := "fresh"
			if previous {
				name = "previous"
			}
			if rollbackFails {
				name += "-rollback-failed"
			}
			t.Run(name, func(t *testing.T) {
				isolateProviderRoot(t)
				coord, _, reg, cleanup := newAsyncTestCoord(t, "vllm")
				defer cleanup()
				marker := fsroot.ProviderVersionFile("vllm")
				require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0700))
				if previous {
					require.NoError(t, os.WriteFile(marker, []byte("old-runtime"), 0600))
				}
				plan := &install.Plan{Provider: "vllm", Action: "install", Recipe: &install.RecipeSnapshot{}}
				plan.Steps = []install.Step{{Number: 1, PostExec: func(context.Context) error {
					if err := os.WriteFile(marker, []byte("candidate-runtime"), 0600); err != nil {
						return err
					}
					panic("synthetic activation panic")
				}}}
				plan.Rollback = func() error {
					if rollbackFails {
						return errors.New("synthetic rollback failure")
					}
					if previous {
						return os.WriteFile(marker, []byte("old-runtime"), 0600)
					}
					return os.Remove(marker)
				}
				id, err := coord.ExecuteResolvedAsync(t.Context(), "vllm", plan, install.PlanOptions{Force: true}, -1, nil)
				require.NoError(t, err)
				sub, err := reg.Subscribe(id, jobs.SubscribeOptions{})
				require.NoError(t, err)
				defer sub.Unsubscribe()
				terminal := waitTerminal(t, sub.Events(), 2*time.Second)
				assert.Equal(t, jobs.PhaseFailed, terminal.Phase)
				rollback, ok := terminal.Meta["rollback"].(jobs.Meta)
				require.True(t, ok, terminal.Meta)
				assert.Equal(t, previous, rollback["previous_runtime"])
				assert.Equal(t, !rollbackFails, rollback["completed"])
				assert.Equal(t, previous && !rollbackFails, rollback["previous_runtime_preserved"])
				if rollbackFails {
					assert.Equal(t, "candidate-runtime", fsroot.ReadInstalledVersion("vllm"))
				} else if previous {
					assert.Equal(t, "old-runtime", fsroot.ReadInstalledVersion("vllm"))
				} else {
					_, err = os.Stat(marker)
					assert.ErrorIs(t, err, os.ErrNotExist)
				}
			})
		}
	}
}
