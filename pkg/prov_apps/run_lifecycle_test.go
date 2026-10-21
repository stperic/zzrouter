package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func lifecycleSchema(t *testing.T) (Option, func(bool)) {
	t.Helper()
	var current atomic.Pointer[schema.YAMLSchema]
	set := func(allow bool) {
		yaml := "parameters:\n  budget: {type: float}\n"
		if allow {
			yaml += "  seed: {type: int}\n"
		}
		parsed, err := schema.LoadYAMLSchema([]byte(yaml))
		require.NoError(t, err)
		current.Store(parsed)
	}
	set(true)
	return WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		declared, errs := schema.Merge(provider, current.Load(), config.ServiceConfig{})
		return declared, errors.Join(errs...)
	}), set
}

func TestRunLifecycleCrashReplay(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	req := pinRequest("crash")
	req.Parameters["budget"] = "0.37"
	req.EnvVars = map[string]string{"PIN_VALUE": "held"}
	run := startPin(t, r.m, req)
	before := readPins(t)
	pid, token := run.ClaimTermination(true)
	require.NoError(t, process.Terminate(t.Context(), process.TerminateRequest{PID: pid, StartToken: token, ProcessGroupID: run.GetProcessGroupID(), Graceful: time.Second}))
	require.Eventually(t, func() bool { return run.GetStatus().IsTerminal() }, contractWait, time.Millisecond)
	require.Equal(t, before, readPins(t))
	restarted := startPin(t, r.m, EnsureRequest{Provider: req.Provider, Model: req.Model, Origin: OriginRequest})
	assert.NotEqual(t, run.ID, restarted.ID)
	assert.Equal(t, req.Parameters, restarted.SnapshotConfig().Parameters)
	assert.Equal(t, req.EnvVars, restarted.SnapshotConfig().EnvVars)
	assert.True(t, restarted.IdleSnapshot().Pinned)
	assert.Equal(t, before, readPins(t))
}

func TestRunLifecycleRefusedRecord(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	bad := pinRequest("refused")
	bad.Parameters["ghost"] = "1"
	seedPins(t, bad)
	r := newContractRig(t, false)
	r.m.Start(t.Context())
	r.m.RestorePinnedRuns()
	r.m.onDemandStarts.Wait()
	listed := r.m.ListInstances("node")
	require.Len(t, listed, 1)
	info := listed[0]
	assert.True(t, info.RecordOnly)
	assert.True(t, info.Pinned)
	assert.Equal(t, instance.StatusStopped, info.Status)
	requireRefused(t, info.Refusal, "ghost", httperr.CodeUnknownFlag)
	assert.Equal(t, bad.Parameters, info.Parameters)
	_, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: bad.Provider, Model: bad.Model, Origin: OriginRequest})
	requireRefused(t, err, "ghost", httperr.CodeUnknownFlag)
	assert.Empty(t, r.m.instances.List())
	require.NoError(t, r.m.StopInstance(t.Context(), info.ID))
	assert.Empty(t, readPins(t))
	assert.Empty(t, r.m.ListInstances("node"))
}

func TestRunLifecycleStopBeforeAcceptance(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	b := newBarrier()
	r := newContractRig(t, false, func(m *ProviderAppManager) { m.seams.admitted = b.hold })
	out := r.ensure(t.Context(), pinRequest("stopped"))
	b.reached(t)
	runs := r.m.instances.List()
	require.Len(t, runs, 1)
	ctx, cancel := context.WithTimeout(t.Context(), contractWait)
	defer cancel()
	require.NoError(t, r.m.StopInstance(ctx, runs[0].ID))
	b.open()
	result := collect(t, []<-chan outcome{out})[0]
	require.NoError(t, result.err)
	assert.Error(t, result.result.Run.Context().Err())
	assert.Empty(t, readPins(t), "a stopped start cannot recreate its pin at acceptance")
}

func TestRunLifecycleConcurrentStarts(t *testing.T) {
	for _, kind := range []string{"pinned", "unpinned leader", "unpinned joiner", "refused flight"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			b := newBarrier()
			opts := []Option{catalogBarrier(b)}
			if kind == "refused flight" {
				opts = append(opts, failFirstLocalize(errTestStart))
			}
			r := newContractRig(t, false, opts...)
			leader := pinRequest("shared")
			leader.Parameters["budget"] = "0.31"
			joiner := pinRequest("shared")
			joiner.Parameters["budget"] = "0.42"
			if kind == "unpinned leader" {
				leader.Parameters["keep-alive"] = "30m"
			}
			if kind == "unpinned joiner" {
				joiner.Parameters["keep-alive"] = "30m"
			}
			ctxs, reqs := same(t.Context(), joiner)
			reqs[0] = leader
			outs := r.leaderAndJoiners(t, b, ctxs, reqs)
			b.open()
			results := collect(t, outs)
			if kind == "refused flight" {
				for _, result := range results {
					require.ErrorIs(t, result.err, errTestStart)
				}
				assert.Equal(t, int32(1), r.attempts.Load())
				assert.Empty(t, readPins(t))
				return
			}
			assert.Equal(t, 1, requireSharedRun(t, results))
			run := results[0].result.Run
			awaitRunning(t, run)
			assert.Equal(t, int32(1), r.attempts.Load())
			assert.Equal(t, leader.Parameters, run.SnapshotConfig().Parameters)
			if kind == "unpinned leader" {
				assert.Empty(t, readPins(t))
			} else {
				saved := readPins(t)
				require.Len(t, saved, 1)
				assert.Equal(t, run.SnapshotConfig().Parameters, saved[0].Parameters)
			}
			reused, err := r.m.EnsureRun(t.Context(), joiner)
			require.NoError(t, err)
			assert.Same(t, run, reused.Run, "D3: an explicit request reuses the live run without rewriting its request")
			assert.Equal(t, leader.Parameters, run.SnapshotConfig().Parameters)
		})
	}
}

func TestRunLifecycleShutdownStoppingKeepsRecord(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	run := startPin(t, r.m, pinRequest("shutdown"))
	before := readPins(t)
	run.TrackGoroutine()
	released := false
	defer func() {
		if !released {
			run.GoroutineDone()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- r.m.Stop(context.Background()) }()
	waitParkedOn(t, ").endRun(", 1, contractWait)
	err := r.m.StopInstance(t.Context(), run.ID)
	require.ErrorIs(t, err, instance.ErrStopping)
	assert.Equal(t, before, readPins(t))
	run.GoroutineDone()
	released = true
	require.NoError(t, <-done)
	assert.Equal(t, before, readPins(t))
}

func TestRunLifecycleRecordDoesNotReserveModel(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := pinRequest("deleted")
	seedPins(t, req)
	r := newContractRig(t, false)
	deleted := false
	require.NoError(t, r.m.WithModelDeletion(t.Context(), func(ctx context.Context) error {
		if err := r.m.CheckModelDeletion(ctx, instance.Weights{Model: req.Model}); err != nil {
			return err
		}
		deleted = true
		return nil
	}))
	assert.True(t, deleted)
	listed := r.m.ListInstances("node")
	require.Len(t, listed, 1)
	assert.True(t, listed[0].RecordOnly)
	assert.Equal(t, req.Model, listed[0].Model)
}

func TestRunLifecyclePrepareBeforeReplacement(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "failed"}[failed], func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			option, set := lifecycleSchema(t)
			r := newContractRig(t, false, option)
			req := pinRequest("protected")
			req.Parameters["seed"] = "7"
			run := startPin(t, r.m, req)
			if failed {
				run.MarkFailedError(errors.New("crashed"))
			}
			before := readPins(t)
			set(false)
			_, err := r.m.RestartInstance(t.Context(), run.ID)
			requireRefused(t, err, "seed", httperr.CodeUnknownFlag)
			registered, exists := r.m.instances.Get(run.ID)
			require.True(t, exists)
			assert.Same(t, run, registered)
			assert.Equal(t, before, readPins(t))
			info := r.m.ListInstances("node")
			require.Len(t, info, 1)
			assert.Equal(t, run.ID, info[0].ID)
			assert.Equal(t, run.GetStatus(), info[0].Status)
			if !failed {
				assert.Equal(t, instance.StatusRunning, run.GetStatus())
			}
		})
	}
}

func TestRunLifecycleMemoryRefusalKeepsRecord(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, true)
			if !known {
				r.before[0].Processes = nil
			}
			before := readPins(t)
			_, err := r.replace(t)
			var refused *process.MemoryFitError
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, before, readPins(t))
			if known {
				registered, ok := r.m.instances.Get(r.old.ID)
				require.True(t, ok)
				assert.Same(t, r.old, registered)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
			} else {
				assert.False(t, r.m.instances.Has(r.old.ID))
				assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
			}
			for _, listed := range r.m.ListInstances("node") {
				if listed.Model == r.old.Model {
					assert.Equal(t, !known, listed.RecordOnly)
					if !known {
						require.ErrorAs(t, listed.Refusal, &refused)
					}
				}
			}
		})
	}
}

func TestRunLifecycleInferenceConfigPinIsNotRecorded(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		svc.Defaults.Parameters["keep-alive"] = "-1"
		return nil
	}))
	run := startPin(t, r.m, EnsureRequest{Provider: "generic", Model: "config-pin", Origin: OriginRequest})
	assert.True(t, run.IdleSnapshot().Pinned)
	assert.Empty(t, readPins(t))
	restarted, err := r.m.RestartInstance(t.Context(), run.ID)
	require.NoError(t, err)
	awaitRunning(t, restarted)
	assert.True(t, restarted.IdleSnapshot().Pinned)
	assert.Equal(t, instance.StartedByRequest, restarted.StartedBy)
	assert.Empty(t, readPins(t))
}

func TestRunLifecycleV1HandleSurvivesBoot(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	seedPins(t, pinRequest("legacy"))
	first := newContractRig(t, false)
	saved := readPins(t)
	require.Len(t, saved, 1)
	require.NotEmpty(t, saved[0].RunID)
	require.NoError(t, first.m.Stop(t.Context()))
	second := newContractRig(t, false)
	second.m.RestorePinnedRuns()
	second.m.onDemandStarts.Wait()
	run, exists := second.m.instances.GetByKey(saved[0].Key())
	require.True(t, exists)
	awaitRunning(t, run)
	restarted, err := second.m.RestartInstance(t.Context(), run.ID)
	require.NoError(t, err)
	awaitRunning(t, restarted)
	kept := readPins(t)
	require.Len(t, kept, 1)
	assert.Equal(t, saved[0].RunID, kept[0].RunID)
	assert.Equal(t, saved[0].Parameters, kept[0].Parameters)
	assert.Equal(t, saved[0].Key(), kept[0].Key())
	require.NoError(t, second.m.Stop(t.Context()))
	third := newContractRig(t, false)
	assert.Equal(t, saved[0].RunID, third.m.ListInstances("node")[0].ID)
}

func TestRunLifecycleExcludedReplay(t *testing.T) {
	for _, kind := range []string{"smoke", "plan"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			req := pinRequest("recorded")
			req.Parameters["budget"] = "0.37"
			req.EnvVars = map[string]string{"PIN_VALUE": "held"}
			seedPins(t, req)
			r := newContractRig(t, false)
			before := readPins(t)
			next := EnsureRequest{Provider: req.Provider, Model: req.Model}
			if kind == "smoke" {
				next.Origin, next.Runtime = OriginInstall, "generic"
			} else {
				next.DisposablePlanID, next.Runtime = strings.Repeat("a", 64), "mlx"
				isolateProviderRoot(t)
				data, err := templates.AppsFS.ReadFile("files/providers/on-demand/mlx/config.yaml")
				require.NoError(t, err)
				var released config.ServiceConfig
				require.NoError(t, yaml.Unmarshal(data, &released))
				svc, ok := r.m.appsConfig.LookupApp("generic")
				require.True(t, ok)
				svc.Name, svc.Install, svc.VersionSource = "mlx", released.Install, released.VersionSource
				svc.Runtime.Execution.Command = "engine"
				cfg := &config.AppsConfig{}
				require.NoError(t, cfg.AddApp("mlx", svc))
				req.Provider, next.Provider = "mlx", "mlx"
				seedPins(t, req)
				manager, err := NewProviderAppManager(cfg, declaredSchema(t, "parameters:\n  budget: {}\n"))
				require.NoError(t, err)
				cleanupProviderManager(t, manager)
				r.m = manager
				before = readPins(t)
				snapshot, err := r.m.resolveRecipe("mlx", "mlx")
				require.NoError(t, err)
				executable := filepath.Join(install.DisposableDir("mlx", next.DisposablePlanID), "venv", "bin", "engine")
				require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0700))
				require.NoError(t, os.WriteFile(executable, []byte(contractEngine), 0700))
				require.NoError(t, os.WriteFile(executable+".ready", nil, 0600))
				require.NoError(t, install.WriteDisposableManifest("mlx", next.DisposablePlanID, install.DisposableManifest{
					Runtime: "mlx", PlanID: next.DisposablePlanID, Recipe: snapshot, RuntimeEnvironmentFingerprint: install.Fingerprint(r.m.providerServiceEnv("mlx")),
				}))
			}
			run := startPin(t, r.m, next)
			assert.False(t, run.IdleSnapshot().Pinned)
			assert.Empty(t, run.SnapshotConfig().Parameters)
			assert.Empty(t, run.SnapshotConfig().EnvVars)
			assert.Equal(t, "0.55", run.Resolved().Parameters["budget"])
			assert.Equal(t, before, readPins(t))
		})
	}
}

func TestRunLifecyclePreviewMatchesReplay(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := pinRequest("preview")
	req.Parameters["budget"] = "0.37"
	req.EnvVars = map[string]string{"PIN_VALUE": "held"}
	seedPins(t, req)
	r := newContractRig(t, false)
	empty := EnsureRequest{Provider: req.Provider, Model: req.Model}
	preview, err := r.m.PreviewLaunch(t.Context(), empty, 0)
	require.NoError(t, err)
	run := startPin(t, r.m, empty)
	assert.Equal(t, preview.LaunchParameters.Params, run.Resolved().Parameters)
	assert.Equal(t, preview.LaunchParameters.Environment, run.Resolved().Environment)
	assert.Equal(t, req.Parameters, run.SnapshotConfig().Parameters)
	assert.True(t, run.IdleSnapshot().Pinned)
}

func TestRunLifecycleQueuedRefusalKeepsRequest(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	saved := pinRequest("queued")
	seedPins(t, saved)
	r := newContractRig(t, true, WithProviderSchema(memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	})), func(m *ProviderAppManager) {
		m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
			return []gpu.MemoryDevice{{UUID: "GPU-test", TotalMiB: 10000, FreeMiB: 6000}}, nil
		}
	})
	before := readPins(t)
	blocker, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "blocker"})
	require.NoError(t, err)
	bad := pinRequest("queued")
	bad.Parameters["ghost"] = "1"
	_, err = r.m.EnsureRun(t.Context(), bad)
	requireRefused(t, err, "ghost", httperr.CodeUnknownFlag)
	assert.Equal(t, before, readPins(t))
	_, exists := r.m.instances.GetByKey(bad.Key())
	assert.False(t, exists, "schema validation precedes queue acceptance")
	r.ready(t)
	awaitRunning(t, blocker.Run)
}

func TestRunLifecycleStopStartRace(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	b := newBarrier()
	r := newContractRig(t, false, func(m *ProviderAppManager) { m.seams.ended = b.hold })
	old := startPin(t, r.m, pinRequest("race"))
	done := make(chan error, 1)
	go func() { done <- r.m.StopInstance(t.Context(), old.ID) }()
	b.reached(t)
	next := pinRequest("race")
	next.Parameters["budget"] = "0.37"
	current := startPin(t, r.m, next)
	b.open()
	require.NoError(t, <-done)
	saved := readPins(t)
	require.Len(t, saved, 1)
	assert.Equal(t, current.SnapshotConfig().Parameters, saved[0].Parameters)
	assert.True(t, r.m.instances.Has(current.ID))
}

func TestRunLifecycleStopRecordDuringReplay(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	seedPins(t, pinRequest("replay"))
	b := newBarrier()
	r := newContractRig(t, false, func(m *ProviderAppManager) { m.seams.admitted = b.hold })
	saved := readPins(t)
	out := r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "replay", Origin: OriginRequest})
	b.reached(t)
	live := r.m.instances.List()
	require.Len(t, live, 1)
	err := r.m.StopInstance(t.Context(), saved[0].RunID)
	require.ErrorIs(t, err, ErrPinRunActive)
	assert.Contains(t, err.Error(), live[0].ID)
	assert.Equal(t, saved, readPins(t))
	b.open()
	result := collect(t, []<-chan outcome{out})[0]
	require.NoError(t, result.err)
	awaitRunning(t, result.result.Run)
	assert.True(t, result.result.Run.IdleSnapshot().Pinned)
	assert.Equal(t, saved, readPins(t))
}

func TestRunLifecycleFailedStopKeepsRecord(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	run := startPin(t, r.m, pinRequest("drain"))
	saved := readPins(t)
	run.TrackGoroutine()
	run.Cancel()
	require.NoError(t, r.m.runEngine(run).down(t.Context(), run))
	budget := r.m.stopBudget
	r.m.stopBudget = func(string) time.Duration { return -1 }
	err := r.m.StopInstance(t.Context(), run.ID)
	run.GoroutineDone()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorIs(t, run.Context().Err(), context.Canceled)
	assert.Equal(t, instance.StatusStopping, run.GetStatus())
	assert.True(t, run.StopUnconfirmed.Load())
	assert.True(t, r.m.instances.Has(run.ID))
	assert.True(t, r.m.ports.IsAllocatedForProvider(run.Provider, run.Port))
	assert.ErrorIs(t, r.m.instances.Register(instance.NewInstance("replacement", run.Provider, run.Model, 0, 0, 0)), instance.ErrModelAlreadyLoaded)
	assert.Equal(t, saved, readPins(t))
	r.m.stopBudget = budget
	require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
	assert.Empty(t, readPins(t))
}

func TestRunLifecycleRestoreInferenceRace(t *testing.T) {
	for _, leader := range []Origin{OriginRestore, OriginRequest} {
		t.Run(map[Origin]string{OriginRestore: "restore leads", OriginRequest: "inference leads"}[leader], func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			req := pinRequest("boot")
			req.Parameters["budget"] = "0.37"
			seedPins(t, req)
			b := newBarrier()
			r := newContractRig(t, false, catalogBarrier(b))
			var out <-chan outcome
			if leader == OriginRestore {
				r.m.RestorePinnedRuns()
				b.reached(t)
				out = r.ensure(t.Context(), EnsureRequest{Provider: req.Provider, Model: req.Model, Origin: OriginRequest})
			} else {
				out = r.ensure(t.Context(), EnsureRequest{Provider: req.Provider, Model: req.Model, Origin: OriginRequest})
				b.reached(t)
				r.m.RestorePinnedRuns()
			}
			waitParked(t, 2, contractWait)
			b.open()
			result := collect(t, []<-chan outcome{out})[0]
			require.NoError(t, result.err)
			r.m.onDemandStarts.Wait()
			run := result.result.Run
			awaitRunning(t, run)
			assert.True(t, run.IdleSnapshot().Pinned)
			assert.Equal(t, req.Parameters, run.SnapshotConfig().Parameters)
			assert.Equal(t, int32(1), r.attempts.Load())
			assert.Len(t, readPins(t), 1)
		})
	}
}

func TestRunLifecycleRefusalBoundary(t *testing.T) {
	for _, path := range []string{"start", "preview"} {
		for _, refusal := range []string{"missing provider", "disabled provider", "invalid target", "schema"} {
			t.Run(path+"/"+refusal, func(t *testing.T) {
				t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
				req := pinRequest("refused")
				if refusal == "missing provider" {
					req.Provider = "missing"
				}
				if refusal == "invalid target" {
					req.Endpoint = EndpointReranking
				}
				if refusal == "schema" {
					req.Parameters["ghost"] = "1"
				}
				seedPins(t, req)
				r := newContractRig(t, false)
				if refusal == "disabled provider" {
					require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(s *config.ServiceConfig) error { s.Enabled = new(false); return nil }))
				}
				empty := EnsureRequest{Provider: req.Provider, Model: req.Model, Endpoint: req.Endpoint, Origin: OriginRequest}
				var err error
				if path == "preview" {
					_, err = r.m.PreviewLaunch(t.Context(), empty, 0)
				} else {
					_, err = r.m.EnsureRun(t.Context(), empty)
				}
				if path == "preview" && refusal == "disabled provider" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				infos := r.m.ListInstances("node")
				require.Len(t, infos, 1)
				if path == "preview" {
					assert.NoError(t, infos[0].Refusal)
				} else {
					require.Equal(t, err, infos[0].Refusal)
				}
			})
		}
	}
}

func TestRunLifecycleRefusalMatchesRequestAndClearsOnAcceptance(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := pinRequest("refusal")
	seedPins(t, req)
	r := newContractRig(t, false)
	bad := pinRequest(req.Model)
	bad.Parameters["ghost"] = "1"
	_, err := r.m.EnsureRun(t.Context(), bad)
	require.Error(t, err)
	assert.NoError(t, r.m.ListInstances("node")[0].Refusal, "another request cannot overwrite the record's last refusal")
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(s *config.ServiceConfig) error { s.Defaults.Parameters["ghost"] = "1"; return nil }))
	_, err = r.m.EnsureRun(t.Context(), EnsureRequest{Provider: req.Provider, Model: req.Model})
	require.Error(t, err)
	require.Error(t, r.m.ListInstances("node")[0].Refusal)
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(s *config.ServiceConfig) error { delete(s.Defaults.Parameters, "ghost"); return nil }))
	run := startPin(t, r.m, EnsureRequest{Provider: req.Provider, Model: req.Model, Origin: OriginRequest})
	require.NoError(t, r.m.instances.Remove(run.ID))
	assert.NoError(t, r.m.ListInstances("node")[0].Refusal, "any accepted start clears the last refusal")
	require.NoError(t, r.m.instances.Register(run))
}

func TestRunLifecycleWholeRequestTier(t *testing.T) {
	for _, kind := range []string{"env only", "pin only"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			saved := pinRequest("whole")
			saved.Parameters["budget"] = "0.37"
			saved.EnvVars = map[string]string{"PIN_VALUE": "old"}
			seedPins(t, saved)
			r := newContractRig(t, false)
			req := EnsureRequest{Provider: saved.Provider, Model: saved.Model}
			if kind == "env only" {
				req.EnvVars = map[string]string{"NEW_VALUE": "new"}
			} else {
				req.Parameters = map[string]string{"keep-alive": "-1"}
			}
			run := startPin(t, r.m, req)
			assert.Equal(t, req.Parameters, run.SnapshotConfig().Parameters)
			assert.Equal(t, req.EnvVars, run.SnapshotConfig().EnvVars)
			assert.Equal(t, "0.55", run.Resolved().Parameters["budget"])
			if kind == "env only" {
				assert.Empty(t, readPins(t))
			} else {
				kept := readPins(t)
				require.Len(t, kept, 1)
				assert.Equal(t, req.Parameters, kept[0].Parameters)
				assert.Empty(t, kept[0].EnvVars)
			}
		})
	}
}

func TestRunLifecycleSmokeRollbackKeepsRecord(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	saved := pinRequest("smoke")
	saved.Parameters["budget"] = "0.37"
	seedPins(t, saved)
	observed := false
	r := newContractRig(t, false, WithHTTPClient(&http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		observed = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: http.Header{}}, nil
	})}))
	before := readPins(t)
	require.NoError(t, r.m.smokeInstalledRuntime(t.Context(), "generic", "generic", saved.Model))
	assert.True(t, observed)
	assert.Empty(t, r.m.instances.List())
	assert.Equal(t, before, readPins(t))
}

func TestRunLifecycleCancelledPreparationPreservesRun(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var armed atomic.Bool
	r := newContractRig(t, false, WithParamLocalizer(func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		if armed.Load() {
			cancel()
		}
		return Localized{Params: params}, nil
	}))
	req := pinRequest("cancel-prepare")
	run := startPin(t, r.m, req)
	before := readPins(t)
	req.Force = true
	prepared, err := r.m.prepareStart(ctx, req)
	require.NoError(t, err)
	prepared.selection, err = r.m.resolveLaunch(&prepared.req)
	require.NoError(t, err)
	prepared.decision, prepared.existing = r.m.decide(ctx, prepared)
	require.Equal(t, run.ID, prepared.decision.Replace)
	armed.Store(true)
	_, err = r.m.executeStart(ctx, prepared)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, r.m.instances.Has(run.ID))
	assert.Equal(t, instance.StatusRunning, run.GetStatus())
	assert.NoError(t, run.Context().Err())
	assert.Equal(t, before, readPins(t))
}

func TestRunLifecycleNonRefusalsKeepLastRefusal(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := pinRequest("non-refusal")
	seedPins(t, req)
	r := newContractRig(t, false)
	r.m.captureRefusal(req, ErrModelNotLocal)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, ErrShutdown} {
		r.m.captureRefusal(req, fmt.Errorf("interrupted: %w", cause))
		assert.ErrorIs(t, r.m.ListInstances("node")[0].Refusal, ErrModelNotLocal)
	}
}

func TestRunLifecycleRecordMapsAreOwned(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := pinRequest("owned")
	req.EnvVars = map[string]string{"PIN_VALUE": "held"}
	seedPins(t, req)
	r := newContractRig(t, false)
	replayed := r.m.replay(EnsureRequest{Provider: req.Provider, Model: req.Model}, LifecycleProcess)
	replayed.Parameters["budget"], replayed.EnvVars["PIN_VALUE"] = "changed", "changed"
	_, found, err := r.m.runOrRecord(readPins(t)[0].RunID)
	require.NoError(t, err)
	found.Parameters["budget"], found.EnvVars["PIN_VALUE"] = "changed", "changed"
	listed := r.m.ListInstances("node")
	require.Len(t, listed, 1)
	assert.Equal(t, req.Parameters, listed[0].Parameters)
	assert.Equal(t, req.EnvVars, listed[0].Environment)
}

func TestRunLifecycleRequestHintRequiresRecord(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprint(recorded), func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			req := pinRequest("hint")
			if recorded {
				seedPins(t, req)
			}
			r := newContractRig(t, false)
			req.Parameters["ghost"] = "1"
			_, err := r.m.PreviewLaunch(t.Context(), req, 0)
			refused := requireRefused(t, err, "ghost", httperr.CodeUnknownFlag)
			hint := refused.ParamIssues()[0].Hint
			assert.Equal(t, recorded, strings.Contains(hint, "pin's whole request tier"))
		})
	}
}

func TestRunLifecycleNodePreparationMatchesStart(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false, declaredSchema(t, "parameters:\n  budget: {}\nendpoints:\n  chat:\n    parameters:\n      budget: {enum: ['999']}\n"))
	fresh := &config.AppsConfig{}
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.RunScope, svc.Protocol, svc.Capabilities = config.RunScopeNode, "", nil
	require.NoError(t, fresh.AddApp("generic", svc))
	r.m.ReloadConfig(fresh)
	preview, err := r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "generic"}, 0)
	require.NoError(t, err)
	run := startPin(t, r.m, EnsureRequest{Provider: "generic"})
	assert.Equal(t, preview.Params, run.Resolved().Parameters)
	assert.Empty(t, run.Endpoint)
	status := r.m.ParametersStatus(t.Context(), run)
	require.Equal(t, instance.ParametersCurrent, status.State, status.Error)
}

func TestRunLifecycleSharedPreparationMatchesStart(t *testing.T) {
	r := newContractRig(t, false, declaredSchema(t, "parameters:\n  budget: {}\nendpoints:\n  embeddings:\n    parameters:\n      embedding-mode: {type: bool}\n"))
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Capabilities = &config.AppCapabilities{WireEndpoints: []string{"chat_completions", "embeddings"}}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", svc))
	r.m.ReloadConfig(cfg)
	req := EnsureRequest{Provider: "generic", Model: "shared", Endpoint: EndpointEmbeddings, Parameters: map[string]string{"embedding-mode": "true"}}
	preview, err := r.m.PreviewLaunch(t.Context(), req, 0)
	require.NoError(t, err)
	run := startPin(t, r.m, req)
	assert.Equal(t, string(EndpointChat), run.Endpoint)
	assert.Equal(t, preview.Params, run.Resolved().Parameters)
	status := r.m.ParametersStatus(t.Context(), run)
	require.Equal(t, instance.ParametersCurrent, status.State, status.Error)
}
