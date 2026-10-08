package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// PrepareInstallPlan resolves the selected runtime from current node config.
func (c *InstallCoordinator) PrepareInstallPlan(ctx context.Context, provider, version string, options install.PlanOptions) (*install.Plan, error) {
	if c.shutdown.Load() {
		return nil, ErrShutdown
	}
	if options.Action != "" && options.Action != "install" && options.Action != "upgrade" {
		return nil, fmt.Errorf("action must be install or upgrade")
	}
	if options.Smoke != nil {
		model := options.Smoke.Model
		if options.Disposable || !fs.ValidPath(model) || filepath.VolumeName(model) != "" || strings.Contains(model, "\\") {
			return nil, fmt.Errorf("smoke requires a registry model name and a normal automatic install")
		}
		if err := process.ValidateModelName(model); err != nil {
			return nil, err
		}
	}
	ctx = install.WithPlanOptions(ctx, options)
	runtime := provider
	if options.Runtime != "" {
		runtime = options.Runtime
	}
	if c.appsConfig != nil && c.appsConfig() != nil {
		sc, ok := c.appsConfig().LookupApp(provider)
		if !ok {
			return nil, ErrProviderNotFound
		}
		if sc.Install != nil {
			if _, ok := sc.Install.Runtimes[runtime]; !ok {
				return nil, fmt.Errorf("runtime not declared by provider")
			}
		} else if options.Runtime != "" && runtime != provider {
			return nil, fmt.Errorf("runtime not declared by provider")
		}
	}
	if err := c.checkPlatform(provider); err != nil {
		return nil, err
	}
	inst, err := c.dispatcher.Get(runtime)
	if err != nil {
		return nil, ErrProviderNotFound
	}
	var plan *install.Plan
	if options.Action == "upgrade" {
		plan, err = inst.UpgradePlan(ctx, version)
	} else {
		plan, err = inst.InstallPlan(ctx, version)
	}
	if err != nil {
		return nil, err
	}
	if options.Smoke != nil && plan.Recipe == nil {
		return nil, fmt.Errorf("smoke requires a typed runtime recipe")
	}
	if plan.Preflight != nil {
		if err := plan.Preflight(ctx); err != nil {
			return nil, err
		}
	}
	if plan.PlanID == "" {
		plan.PlanID = install.Fingerprint(plan)
	}
	plan.BindOptions(options)
	if options.ExpectedPlanID != "" && options.ExpectedPlanID != plan.PlanID {
		return nil, install.ErrStalePlan
	}
	return plan, nil
}

// ExecuteResolvedAsync executes and finalizes the immutable accepted plan under its runtime lock.
func (c *InstallCoordinator) ExecuteResolvedAsync(ctx context.Context, provider string, plan *install.Plan, options install.PlanOptions, step int, finalize func() error) (string, error) {
	if c.shutdown.Load() {
		return "", ErrShutdown
	}
	if c.jobs == nil {
		return "", ErrAsyncDisabled
	}
	if plan.Recipe == nil {
		return "", fmt.Errorf("resolved execution requires a declared Python runtime")
	}
	if step >= 0 && options.ExpectedPlanID == "" {
		return "", fmt.Errorf("expected_plan_id is required for guided execution")
	}
	if options.Smoke != nil && (options.Disposable || step >= 0 || c.smoke == nil) {
		return "", fmt.Errorf("smoke requires a normal automatic install with runs support")
	}
	if options.Disposable && step >= 0 {
		return "", fmt.Errorf("disposable installation requires automatic execution")
	}
	if options.ExpectedPlanID != "" && options.ExpectedPlanID != plan.PlanID {
		return "", install.ErrStalePlan
	}
	if c.runtimeInUse(plan.Provider) {
		return "", ErrInstancesRunning
	}
	installer, err := c.dispatcher.Get(plan.Provider)
	if err != nil {
		return "", ErrProviderNotFound
	}
	if step < 0 && installer.IsInstalled() && !options.Force && !options.Disposable && plan.Action != "upgrade" {
		return "", ErrProviderAlreadyInstalled
	}
	if plan.Action == "upgrade" && !installer.IsInstalled() {
		return "", ErrProviderNotFound
	}
	h := c.openJob(plan.Action, provider)
	if h == nil {
		return "", ErrAsyncDisabled
	}
	if existing, busy := c.reserveJob(plan.Provider, h.ID()); busy {
		h.Done()
		return "", fmt.Errorf("%w: runtime install job %s is already running", ErrInstancesRunning, existing)
	}
	h.Meta(jobs.Meta{"runtime": plan.Provider, "plan_id": plan.PlanID, "recipe_fingerprint": plan.Recipe.Fingerprint, "policy_fingerprint": plan.Recipe.PolicyFingerprint})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.releaseJob(plan.Provider, h.ID())
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("recipe execution panic", "panic", recovered, "stack", string(debug.Stack()))
				h.Fail(fmt.Errorf("recipe execution panic: %v", recovered))
			}
		}()
		if err := c.runResolvedPlan(h, provider, plan, options, step, finalize); err != nil {
			h.Fail(err)
			return
		}
		h.Done()
	}()
	return h.ID(), nil
}

func (c *InstallCoordinator) runResolvedPlan(h jobs.Handle, provider string, plan *install.Plan, options install.PlanOptions, step int, finalize func() error) (resultErr error) { //nolint:gocyclo,cyclop // Step execution, install finalization and failure recovery share the provider lock.
	ctx := h.Context()
	unlock, err := c.acquireProvider(ctx, plan.Provider)
	if err != nil {
		return err
	}
	defer unlock()
	if c.shutdown.Load() {
		return ErrShutdown
	}
	if c.runtimeInUse(plan.Provider) {
		return ErrInstancesRunning
	}
	if plan.Action == "upgrade" {
		installer, getErr := c.dispatcher.Get(plan.Provider)
		if getErr != nil || !installer.IsInstalled() {
			return ErrProviderNotFound
		}
	}
	lock := fsroot.NewLockFile(plan.Provider)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	hadPrevious := fsroot.ReadInstalledVersion(plan.Provider) != ""
	defer recoverRecipePanic(h, plan, hadPrevious, &resultErr)
	if step >= 0 {
		fresh, err := c.PrepareInstallPlan(ctx, provider, plan.Version, options)
		if err != nil {
			return err
		}
		if fresh.PlanID != plan.PlanID {
			return install.ErrStalePlan
		}
	}
	progress := c.progress.Start(plan.Provider, plan.Action, len(plan.Steps))
	progress.SetJobHandle(progressOnlyHandle{inner: h})
	if step >= 0 {
		result := plan.ExecuteStep(ctx, step)
		h.Meta(jobs.Meta{"result": result})
		if !result.Passed {
			err = fmt.Errorf("%s", result.Message)
		}
	} else {
		err = plan.Execute(ctx, func(message string) {
			if n, total, description, ok := parseStepProgress(message); ok {
				progress.SetStep(n, total, description)
			}
		})
	}
	if err == nil && step >= 0 && step != len(plan.Steps) {
		progress.SetCompleted()
		return nil
	}
	if err == nil && !plan.VerifyAllContext(ctx).AllOK {
		err = fmt.Errorf("completion verification failed")
	}
	var busySmoke error
	if err == nil && options.Smoke != nil {
		h.Meta(jobs.Meta{"smoke_model": options.Smoke.Model})
		err = c.smoke(ctx, provider, plan.Provider, options.Smoke.Model)
		if err == nil {
			h.Meta(jobs.Meta{"smoke_status": "passed"})
		} else if errors.Is(err, errSmokeBusy) {
			busySmoke, err = err, nil
			h.Meta(jobs.Meta{"smoke_status": "skipped_busy"})
		} else {
			h.Meta(jobs.Meta{"smoke_status": "failed"})
		}
		if err != nil {
			err = fmt.Errorf("install smoke: %w", err)
		}
		if errors.Is(err, errSmokeRollbackBlocked) {
			h.Meta(jobs.Meta{"smoke_status": "rollback_blocked", "rollback": jobs.Meta{"completed": false, "blocked": true}})
			progress.SetFailed(err.Error())
			return err
		}
	}
	if err == nil && finalize != nil && !plan.Disposable {
		if finalizeErr := finalize(); finalizeErr != nil {
			err = fmt.Errorf("finalize: %w", finalizeErr)
		}
	}
	if err != nil {
		rollbackErr := error(nil)
		if plan.Rollback != nil {
			rollbackErr = plan.Rollback()
		}
		h.Meta(jobs.Meta{"rollback": jobs.Meta{"previous_runtime": hadPrevious, "completed": rollbackErr == nil, "previous_runtime_preserved": hadPrevious && rollbackErr == nil}})
		progress.SetFailed(err.Error())
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback failed: %w", rollbackErr))
		}
		if hadPrevious {
			return fmt.Errorf("%w; previous runtime preserved or restored", err)
		}
		return err
	}
	if version, err := installerVersion(c, plan.Provider); err == nil && !plan.Disposable {
		c.versions.Set(plan.Provider, version)
	}
	if busySmoke != nil {
		h.Meta(jobs.Meta{"installed": true, "warning": "Smoke skipped: resources busy; the verified runtime was kept."})
	}
	progress.SetCompleted()
	return nil
}

func recoverRecipePanic(h jobs.Handle, plan *install.Plan, hadPrevious bool, resultErr *error) {
	if recovered := recover(); recovered != nil {
		slog.Error("recipe execution panic", "panic", recovered, "stack", string(debug.Stack()))
		*resultErr = fmt.Errorf("recipe execution panic: %v", recovered)
		var rollbackErr error
		if plan.Rollback != nil {
			rollbackErr = plan.Rollback()
		}
		h.Meta(jobs.Meta{"rollback": jobs.Meta{"previous_runtime": hadPrevious, "completed": rollbackErr == nil, "previous_runtime_preserved": hadPrevious && rollbackErr == nil}})
		*resultErr = errors.Join(*resultErr, rollbackErr)
	}
}

func installerVersion(c *InstallCoordinator, runtime string) (string, error) {
	installer, err := c.dispatcher.Get(runtime)
	if err != nil {
		return "", err
	}
	return installer.InstalledVersion()
}

// runtimeProvider maps a feature's managed directory back to its launch owner.
func (c *InstallCoordinator) runtimeProvider(runtime string) string {
	if c.appsConfig != nil && c.appsConfig() != nil {
		for name, p := range c.appsConfig().OnDemandProviders() {
			if p.Install != nil {
				if _, ok := p.Install.Runtimes[runtime]; ok {
					return name
				}
			}
		}
	}
	return runtime
}

func (c *InstallCoordinator) runtimeInUse(runtime string) bool {
	for _, run := range c.instances.ListAll() {
		selected := run.Resolved().Runtime
		if selected == "" {
			selected = run.SnapshotConfig().Runtime
		}
		if selected == "" {
			selected = run.Provider
		}
		if selected == runtime && (run.StopUnconfirmed.Load() || !run.GetStatus().IsTerminal()) {
			return true
		}
	}
	return false
}
