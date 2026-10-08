package servercli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/version"
)

// allowConfigSourceEnv opts a privileged run into honouring
// update.source from node.yaml.
//
// Off by default because node.yaml is owned by the service user -- it
// has to be, since the node rewrites it on cluster join and leave -- and
// update.source names the feed a binary comes from. Honouring it here
// would let anything that compromised the node process choose what root
// installs, which is precisely the escalation this whole split exists to
// prevent.
//
// An environment variable rather than a config key because on a service
// install the only place it can be set is the unit file, and that is
// root-owned. The permission to redirect the feed lands at the same
// trust level as the permission to change what the updater runs.
const allowConfigSourceEnv = "ZZROUTER_UPDATE_ALLOW_CONFIG_SOURCE"

// updateRunProgressInterval is how often a run republishes what it is
// doing, for an API caller watching the job.
const updateRunProgressInterval = 500 * time.Millisecond

// newUpdateRunCmd creates the privileged update run command.
func newUpdateRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Perform a privileged update run (started by systemd)",
		Long: `Perform one update run with the privileges needed to replace the node's binaries.

Started by systemd, not by hand: zzrouter-update.path when the node asks for
an update, and zzrouter-update.timer for the scheduled check. Running it
manually as root does the same thing and is a reasonable way to debug.

The node itself cannot do this work. It runs as the unprivileged zzrouter
user and its install tree is root-owned, deliberately: a service account
that can rewrite the binary an operator later runs under sudo turns a
compromise of the node process into root on the machine.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrivilegedUpdate(cmd.Context())
		},
	}
}

// runPrivilegedUpdate performs one run and publishes what happened.
func runPrivilegedUpdate(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("update run replaces the node's binaries and must run as root; " +
			"systemd starts it via zzrouter-update.service")
	}

	handoff := update.DefaultHandoff()

	// Claimed before any other work, so a request that fails to parse or
	// a run that dies partway cannot have the path unit start it again
	// in a loop.
	req, err := handoff.Claim()
	if err != nil {
		return fmt.Errorf("read the update request: %w", err)
	}
	fromTimer := req == nil
	if req == nil {
		// No request waiting: this is the timer, not the node.
		req = &update.Request{Action: update.ActionCheck, RequestedBy: "timer"}
	}
	return executeUpdateRequest(ctx, handoff, req, fromTimer, pkgConfig.NewConfigManager("zzrouter").LoadNodeConfig)
}

func executeUpdateRequest(ctx context.Context, handoff *update.Handoff, req *update.Request, fromTimer bool, load func() (*pkgConfig.NodeConfig, error)) (runErr error) {
	defer func() {
		if runErr != nil {
			finished := utils.NowUTC()
			_ = handoff.Publish(&update.RunStatus{PID: os.Getpid(), JobID: req.JobID, Action: req.Action, State: update.StateFailed, ToVersion: req.TargetVersion, StartedAt: req.RequestedAt, FinishedAt: &finished, Error: runErr.Error()})
		}
	}()

	cfg, err := load()
	if err != nil {
		return fmt.Errorf("load node config: %w", err)
	}
	if fromTimer && !cfg.Update.IsEnabled() {
		return nil
	}
	applyPrivilegedSourcePolicy(&cfg.Update)
	if err := cfg.Update.Validate(); err != nil {
		return fmt.Errorf("invalid update config: %w", err)
	}

	restart := &deferredRestart{}
	run := &privilegedRun{
		handoff: handoff,
		req:     req,
		restart: restart,
		probe:   nodeProbe{cfg: cfg},
		manager: service.NewServiceManager(),
		// A ROOT-OWNED working directory, not the node's config
		// directory. That one is /etc/zzrouter and is owned by the
		// service user -- it has to be, since the node rewrites its own
		// config on cluster join -- so a root process writing downloads,
		// backups and history there is writing into a directory an
		// unprivileged principal controls.
		sched: update.NewSchedulerIn(update.DefaultStatusDir(), &cfg.Update, clock.System(),
			update.WithRestarter(restart)),
	}
	return run.execute(ctx)
}

// applyPrivilegedSourcePolicy strips a release-feed override that the
// service user could have written. See allowConfigSourceEnv.
func applyPrivilegedSourcePolicy(cfg *pkgConfig.UpdateConfig) {
	if cfg.Source == nil {
		return
	}
	if os.Getenv(allowConfigSourceEnv) == "1" {
		slog.Warn("honouring a release feed from node.yaml because "+allowConfigSourceEnv+" is set",
			"source", cfg.Source.Describe())
		return
	}
	slog.Warn("ignoring update.source from node.yaml: a privileged run uses the shipped release feed",
		"ignored", cfg.Source.Describe(),
		"override_with", allowConfigSourceEnv+"=1 in the unit file")
	cfg.Source = nil
}

// deferredRestart holds the scheduler's restart request rather than
// acting on it.
//
// The scheduler's own restarter exits the process so a supervisor
// starts the node again -- correct when the node is updating itself,
// meaningless here: this process is the updater, not the node, and
// exiting would restart the wrong thing. So the request is recorded and
// the run restarts the node's unit afterwards, where it can also wait
// and see whether the new version serves.
type deferredRestart struct{ requested atomic.Bool }

func (d *deferredRestart) Restart() error {
	d.requested.Store(true)
	return nil
}

func (d *deferredRestart) wanted() bool { return d.requested.Load() }

// privilegedRun is one execution of the updater.
type privilegedRun struct {
	handoff *update.Handoff
	req     *update.Request
	sched   *update.Scheduler
	restart *deferredRestart
	probe   nodeProbe
	manager service.ServiceManager

	// status is touched by two goroutines: the run itself, and the
	// mirror that republishes progress while a download is in flight.
	// Guarded rather than handed off, because both genuinely have
	// something to say -- the run knows the version, the mirror knows
	// how far the download got.
	mu     sync.Mutex
	status *update.RunStatus
}

// withStatus mutates the published status under the lock.
func (r *privilegedRun) withStatus(fn func(s *update.RunStatus)) {
	r.mu.Lock()
	fn(r.status)
	r.mu.Unlock()
}

func (r *privilegedRun) execute(ctx context.Context) error {
	r.status = &update.RunStatus{
		PID:         os.Getpid(),
		Action:      r.req.Action,
		State:       update.StateChecking,
		JobID:       r.req.JobID,
		FromVersion: version.Current.String(),
		ToVersion:   r.req.TargetVersion,
		StartedAt:   utils.NowUTC(),
	}
	r.publish()

	slog.Info("privileged update run starting",
		"action", r.req.Action, "requested_by", r.req.RequestedBy, "job_id", r.req.JobID)

	stopMirror := r.mirrorProgress(ctx)
	err := r.perform(ctx)
	stopMirror()

	finished := utils.NowUTC()
	r.withStatus(func(s *update.RunStatus) {
		s.FinishedAt = &finished
		s.Success = err == nil
		s.State = update.StateIdle
		if err != nil {
			s.State = update.StateFailed
			s.Error = err.Error()
		}
	})
	r.publish()

	if err != nil {
		slog.Error("privileged update run failed", "action", r.req.Action, "err", err)
		return err
	}
	slog.Info("privileged update run finished", "action", r.req.Action)
	return nil
}

// perform dispatches the action. Every branch is reached only with the
// request already claimed and validated.
func (r *privilegedRun) perform(ctx context.Context) error {
	switch r.req.Action {
	case update.ActionCheck:
		return r.check(ctx)
	case update.ActionApply:
		return r.apply(ctx)
	case update.ActionRollback:
		return r.rollback(ctx)
	default:
		return fmt.Errorf("%w: %q", update.ErrUnknownAction, r.req.Action)
	}
}

// check asks the release feed what is available. It does not install:
// whether a check turns into an apply is the node's decision, made
// against its own config and maintenance window, and it says so by
// leaving a request.
func (r *privilegedRun) check(ctx context.Context) error {
	result, err := r.sched.CheckNow(ctx)
	if err != nil {
		return err
	}
	if !result.UpdateAvailable {
		slog.Info("no update available", "current", result.CurrentVersion.String())
		return nil
	}
	latest := result.LatestRelease.Version.String()
	r.withStatus(func(s *update.RunStatus) { s.ToVersion = latest })
	slog.Info("an update is available", "version", latest)
	return nil
}

// apply installs the pending release and then proves the node still
// serves on it, rolling back when it does not.
func (r *privilegedRun) apply(ctx context.Context) error {
	var err error
	if r.req.TargetVersion != "" {
		err = r.sched.ApplyVersion(ctx, r.req.TargetVersion)
	} else {
		err = r.sched.ApplyNow(ctx)
	}
	if err != nil {
		return err
	}
	if !r.restart.wanted() {
		// auto_restart is off: the binaries are in place and the
		// operator restarts when they choose. There is nothing here to
		// verify yet, and nothing to roll back.
		slog.Info("update installed; waiting for a restart to take effect (auto_restart is disabled)")
		return nil
	}
	return r.restartAndVerify(ctx, true)
}

// rollback returns the node to its previous version and restarts it.
func (r *privilegedRun) rollback(ctx context.Context) error {
	result, err := r.sched.Rollback(ctx)
	if err != nil {
		return err
	}
	if result != nil {
		r.withStatus(func(s *update.RunStatus) { s.ToVersion = result.Version })
	}
	// Verifying without a second rollback: there is nowhere further
	// back to go that this run should decide on its own.
	return r.restartAndVerify(ctx, false)
}

// restartAndVerify restarts the node and waits for it to serve.
//
// This is the whole reason the work moved to a privileged process. When
// the node updated itself it had to exit and let the supervisor bring
// it back, so nothing was left running to notice that it never did --
// the check had to be reconstructed across the process boundary from a
// file and a boot counter. Here the updater outlives the restart and
// can simply watch.
func (r *privilegedRun) restartAndVerify(ctx context.Context, mayRollBack bool) error {
	r.withStatus(func(s *update.RunStatus) { s.State = update.StateRestarting })
	r.publish()

	if err := r.manager.RestartService(); err != nil {
		return fmt.Errorf("restart the node: %w", err)
	}

	served := r.probe.waitUntilServing(ctx.Done())
	if served == nil {
		if err := r.sched.ConfirmUpdate(); err != nil {
			slog.Warn("the node is serving but the pending update record could not be cleared", "err", err)
		}
		slog.Info("the node is serving on the new version")
		return nil
	}

	if !mayRollBack {
		return fmt.Errorf("the node did not come back: %w", served)
	}

	slog.Error("the node did not come back after the update; rolling back", "reason", served)
	result, rbErr := r.sched.Rollback(ctx)
	if rbErr != nil {
		return fmt.Errorf("the node did not come back (%w) and could not be rolled back: %w", served, rbErr)
	}
	if result != nil {
		r.withStatus(func(s *update.RunStatus) { s.ToVersion = result.Version })
	}
	if err := r.manager.RestartService(); err != nil {
		return fmt.Errorf("the node did not come back (%w) and the restore could not be started: %w", served, err)
	}
	if err := r.probe.waitUntilServing(ctx.Done()); err != nil {
		return fmt.Errorf("the node did not come back (%w) and neither did the version restored after it: %w", served, err)
	}
	return fmt.Errorf("the update did not come up (%w); rolled back and the node is serving again", served)
}

// mirrorProgress republishes the scheduler's status while a run is in
// flight, so an API caller watching the job sees download and verify
// progress from a process it cannot see. Returns a function that stops
// the mirror and takes a last reading.
func (r *privilegedRun) mirrorProgress(ctx context.Context) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(updateRunProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sample()
				r.publish()
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// sample copies what the scheduler is doing into the published status.
func (r *privilegedRun) sample() {
	status := r.sched.GetStatus()
	r.withStatus(func(s *update.RunStatus) {
		s.State = status.State
		s.Phase = string(status.State)
		s.Progress = status.DownloadProgress
		if status.LatestVersion != nil {
			s.ToVersion = status.LatestVersion.String()
		}
	})
}

// publish records the run for the node to read back. A failure here
// costs the reporting, not the run: an update that succeeded while its
// status file could not be written is still an update that succeeded.
// Publishing takes the same lock and marshals under it, so a status
// written from the mirror goroutine can never be a half-updated view of
// one the run was in the middle of changing.
func (r *privilegedRun) publish() {
	r.mu.Lock()
	err := r.handoff.Publish(r.status)
	r.mu.Unlock()
	if err != nil {
		slog.Warn("could not publish the update run status", "err", err)
	}
}
