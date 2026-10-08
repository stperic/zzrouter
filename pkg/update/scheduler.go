package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/version"
)

// ErrApplyInFlight is returned by ApplyNow / ApplyNowAsync when another
// update apply is already running. performCheck silently no-ops
// on contention (timer-driven; the next tick can retry).
var ErrApplyInFlight = errors.New("an update apply is already in flight")

// Scheduler manages periodic update checks and application
type Scheduler struct {
	enabled    atomic.Bool
	checkMu    sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	started    bool
	config     *config.UpdateConfig
	checker    *Checker
	downloader *Downloader
	verifier   *Verifier
	installer  *Installer
	history    *History
	confirmer  *Confirmer
	clock      clock.Clock

	// Current state
	status UpdateStatus
	mu     sync.RWMutex

	// Lifecycle management
	stopChan      chan struct{}
	stopped       bool
	initialJitter time.Duration // Set in Start; consumed by schedulerLoop
	// wg tracks the owned schedulerLoop goroutine. Stop blocks on
	// wg.Wait after closing stopChan so callers get clean shutdown
	// ordering (Scheduler.Stop must finish before its collaborators
	// are torn down).
	wg sync.WaitGroup

	// delegate is set when this node cannot install its own updates and
	// must ask the privileged updater instead. Nil on every install
	// where the node owns its own binaries.
	delegate      *Handoff
	operationPath string
	operationMu   sync.Mutex
	acceptMu      sync.Mutex

	// restarter brings the node back on the newly-installed binary
	// after a successful apply. Never nil — NewScheduler installs the
	// supervisor-backed one and WithRestarter only replaces it.
	restarter Restarter

	// Directories
	tempDir   string
	backupDir string

	// Optional jobs registry. When non-nil, every applyUpdate opens a
	// KindUpdate handle and mirrors download/verify/install progress
	// onto the SSE stream.
	jobsRegistry *jobs.Registry

	// applying is the single-flight gate across the three apply entry
	// points (performCheck → auto-apply, ApplyNow, ApplyNowAsync). A
	// CAS-claimed true blocks the other two; the claimant clears it on
	// exit. Pre-existing race: scheduler tick colliding with an HTTP
	// ApplyNowAsync can double-download + double-install the same
	// binary.
	applying atomic.Bool
}

// beginApply claims the apply gate. Returns true when the caller owns
// the apply window and must call endApply on exit; false when another
// apply is in flight and the caller must bail.
func (s *Scheduler) beginApply() bool { return s.applying.CompareAndSwap(false, true) }

func (s *Scheduler) endApply() { s.applying.Store(false) }

// SchedulerOption is a function that configures a Scheduler
type SchedulerOption func(*Scheduler)

// WithRestarter replaces the supervisor-backed restarter. Tests use it
// to observe the restart without exiting the test binary.
func WithRestarter(r Restarter) SchedulerOption {
	return func(s *Scheduler) {
		if r != nil {
			s.restarter = r
		}
	}
}

// WithJobsRegistry enables SSE streaming for every applyUpdate. Each
// apply opens a KindUpdate handle; download/verify/install progress
// mirrors onto the stream at /zzrouter/v1/jobs/:id/stream.
func WithJobsRegistry(r *jobs.Registry) SchedulerOption {
	return func(s *Scheduler) {
		s.jobsRegistry = r
	}
}

// NewScheduler creates a new update scheduler. The Clock must be
// non-nil; production callers pass clock.System(), tests pass a
// clocktest.FakeClock.
func NewScheduler(cfg *config.UpdateConfig, clk clock.Clock, opts ...SchedulerOption) *Scheduler {
	return NewSchedulerIn("", cfg, clk, opts...)
}

// NewSchedulerIn builds a scheduler whose downloads, backups, history
// and pending-confirm record live under workDir.
//
// The privileged updater must pass one, and it must be root-owned. Left
// empty the working directory is the node's config directory, which on
// a service install is /etc/zzrouter -- owned by the SERVICE USER,
// because the node rewrites its own config on cluster join and leave.
// A root process writing there is writing into a directory an
// unprivileged principal controls, so a planted symlink redirects root's
// write anywhere it can reach. Fine for the node, which owns that
// directory and is the principal in question; not fine for root.
func NewSchedulerIn(workDir string, cfg *config.UpdateConfig, clk clock.Clock, opts ...SchedulerOption) *Scheduler {
	if clk == nil {
		panic("update: NewScheduler requires a non-nil clock.Clock")
	}

	// Get data directory for temp and backup storage
	dataDir := workDir
	if dataDir == "" {
		dataDir = config.NewConfigManager("zzrouter").GetNodeConfigDir()
	}

	tempDir := filepath.Join(dataDir, "update-temp")
	backupDir := filepath.Join(dataDir, "backups")
	historyFile := filepath.Join(dataDir, "update-history.json")

	cfgCopy := *cfg
	cfg = &cfgCopy
	ownedCtx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		ctx: ownedCtx, cancel: cancel,
		config:     cfg,
		checker:    NewChecker(cfg.GetChannel(), cfg.PinnedVersion),
		downloader: NewDownloader(tempDir),
		verifier:   NewVerifier(DefaultOwner, DefaultRepo),
		installer:  NewInstaller(backupDir, cfg.GetKeepPreviousVersions()),
		history:    NewHistory(historyFile),
		confirmer:  NewConfirmerIn(dataDir, cfg.GetKeepPreviousVersions()),
		restarter:  newSupervisorRestarter(),
		clock:      clk,
		status: UpdateStatus{
			State:          StateIdle,
			CurrentVersion: version.Current,
			Channel:        cfg.GetChannel(),
		},
		stopChan:      make(chan struct{}),
		tempDir:       tempDir,
		backupDir:     backupDir,
		operationPath: filepath.Join(dataDir, "update-operation.json"),
	}

	s.enabled.Store(cfg.IsEnabled())

	// A non-default release feed decides which binary this node will run
	// next, so it is announced rather than applied quietly: once in the
	// log at construction, and on every read of the update status.
	if cfg.Source != nil {
		s.checker.owner, s.checker.repo = cfg.Source.Owner, cfg.Source.Repo
		s.checker.SetAPIBaseURL(cfg.Source.APIBaseURL)
		s.downloader.SetAllowedHosts(cfg.Source.AllowedHosts)
		s.downloader.SetAllowLoopbackHTTP(cfg.Source.IsLoopbackOnly())
		s.status.Source = cfg.Source.Describe()
		slog.Warn("updates are configured to come from a non-default source",
			"source", s.status.Source,
			"allowed_hosts", cfg.Source.AllowedHosts,
			"plaintext_loopback", cfg.Source.IsLoopbackOnly())
	}

	// Apply options
	for _, opt := range opts {
		opt(s)
	}

	// Answer "could this node install an update at all" once, at
	// construction, so it shows up in status rather than as a failed
	// apply months later when a release finally lands.
	//
	// Two very different reasons to answer no. On a managed install the
	// tree is root-owned by design and the node is supposed to ask the
	// privileged updater — that is delegation, not an obstruction, and
	// reporting it as Blocked would have an operator "fix" the one
	// property holding the privilege split up. Anything else really is
	// stuck and says so.
	if err := s.installer.CheckWritable(); err != nil {
		if s.installer.IsManaged() {
			s.delegate = DefaultHandoff()
			slog.Info("updates on this node are performed by the privileged updater",
				"request_via", s.delegate.RequestPath())
		} else {
			s.status.Blocked = err.Error()
			slog.Warn("this node cannot install updates", "err", err)
		}
	}

	return s
}

// Delegated returns the handoff to the privileged updater when this
// node must ask rather than install, and nil when it installs its own
// updates.
//
// Callers that start work have to branch on it: what comes back from a
// delegated apply is an acknowledgement, not a result, and reporting it
// as one would claim an update that has not happened yet.
func (s *Scheduler) Delegated() *Handoff { return s.delegate }

// RequestPrivileged asks the privileged updater to do something, on
// behalf of an API or CLI caller who asked this node.
func (s *Scheduler) RequestPrivileged(action RequestAction, requestedBy string) error {
	if s.delegate == nil {
		return fmt.Errorf("this node installs its own updates; there is no privileged updater to ask")
	}
	target := ""
	s.mu.RLock()
	if s.status.PendingRelease != nil {
		target = s.status.PendingRelease.Version.String()
	}
	s.mu.RUnlock()

	return s.delegate.Submit(&Request{
		Action:        action,
		TargetVersion: target,
		RequestedBy:   requestedBy,
		RequestedAt:   s.clock.Now().UTC(),
	})
}

// Start starts the update scheduler background goroutine
func (s *Scheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	context.AfterFunc(ctx, s.cancel)

	// Compute the initial jitter here so we can log it alongside the
	// "started" line. The schedulerLoop receives it via the field so the
	// two stay in sync without re-rolling the random value.
	//
	// TODO(tier1.1.i): expose via WithInitialJitter option. FakeClock
	// tests of schedulerLoop must currently Advance up to 60m to pass
	// this jitter before the ticker fires; a deterministic override
	// would let tests set it to 0.
	s.initialJitter = time.Duration(rand.Int63n(int64(60 * time.Minute)))
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.schedulerLoop(s.ctx)
	}()
	slog.Info("update scheduler started",
		"channel", s.config.GetChannel(),
		"interval_hours", s.config.GetCheckIntervalHours(),
		"first_check_in", s.initialJitter,
	)
}

// Stop stops the scheduler. Idempotent. Blocks until the owned
// schedulerLoop goroutine exits — the lock is dropped before wg.Wait
// so the loop can acquire s.mu for its final status updates.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.cancel()
	close(s.stopChan)
	s.mu.Unlock()

	s.wg.Wait()
	slog.Info("update scheduler stopped")
}

// schedulerLoop is the main scheduler loop
func (s *Scheduler) schedulerLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC recovered in update scheduler",
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	// Initial jitter (0-60 minutes, computed in Start) avoids thundering
	// herd when multiple nodes restart simultaneously. Logged once in Start.
	select {
	case <-s.clock.After(s.initialJitter):
	case <-s.stopChan:
		return
	case <-ctx.Done():
		return
	}

	// Perform initial check
	s.performCheck(ctx)

	// Calculate check interval
	interval := time.Duration(s.config.GetCheckIntervalHours()) * time.Hour

	// Schedule next check
	s.setNextCheckTime(s.clock.Now().Add(interval))

	ticker := s.clock.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.Chan():
			s.performCheck(ctx)
			s.setNextCheckTime(s.clock.Now().Add(interval))

		case <-s.stopChan:
			return

		case <-ctx.Done():
			return
		}
	}
}

// performCheck checks for updates and applies if in maintenance window
func (s *Scheduler) performCheck(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	s.setState(StateChecking)

	// Track metrics for update check
	checkRecorder := NewUpdateCheckRecorder(ctx)

	s.checkMu.Lock()
	result, err := s.checker.Check(ctx)
	s.checkMu.Unlock()
	if err != nil {
		utils.LogWarnf("Update check failed: %v", err)
		// Record check failure in metrics
		status := UpdateStatusFailed
		if _, ok := err.(*RateLimitError); ok {
			status = UpdateStatusRateLimited
		}
		checkRecorder.RecordCheckResult(status, false)
		s.setError(err)
		return
	}

	s.setLastCheckTime(s.clock.Now())

	if !result.UpdateAvailable {
		slog.Info("no update available", "current", version.Current.String())
		checkRecorder.RecordCheckResult(UpdateStatusSuccess, false)
		s.setState(StateIdle)
		return
	}

	slog.Info("update available",
		"current", version.Current.String(),
		"latest", result.LatestRelease.Version.String(),
	)
	checkRecorder.RecordCheckResult(UpdateStatusSuccess, true)

	// Track pending update in metrics
	metrics := GetUpdateMetrics()
	if metrics != nil {
		metrics.SetPendingUpdate(ctx, true)
	}

	s.mu.Lock()
	s.status.UpdateAvailable = true
	s.status.LatestVersion = result.LatestRelease.Version
	s.status.PendingRelease = result.LatestRelease
	s.mu.Unlock()

	// Check maintenance window
	if s.config.HasMaintenanceWindow() {
		if !s.isInMaintenanceWindow() {
			slog.Info("update pending - waiting for maintenance window")
			s.setState(StateWaitingForWindow)
			return
		}
		slog.Info("in maintenance window - proceeding with update")
	}

	// Apply update. Timer-driven path bails quietly if an HTTP caller
	// already started an apply; the next tick retries.
	if !s.Enabled() {
		s.setState(StateIdle)
		return
	}
	if !s.beginApply() {
		slog.Info("scheduled update apply skipped: another apply is in flight")
		s.setState(StateIdle)
		return
	}
	defer s.endApply()
	if s.delegate != nil {
		if err := s.RequestPrivileged(ActionApply, "scheduled update"); err != nil {
			s.setError(err)
		}
		return
	}

	h := s.openUpdateJob(ctx, result.LatestRelease)
	if err := s.applyUpdate(ctx, result.LatestRelease, h); err != nil {
		slog.Error("failed to apply update", "err", err)
		s.setError(err)
		if h != nil {
			h.Fail(err)
		}
		return
	}
	if h != nil {
		h.Done()
	}
}

// openUpdateJob opens a KindUpdate handle bound to ctx (sync ApplyNow
// path — caller cancellation propagates). Returns nil when the jobs
// integration is disabled or Start fails. Safe to pass nil into applyUpdate.
func (s *Scheduler) openUpdateJob(ctx context.Context, release *ReleaseInfo) jobs.Handle {
	if s.jobsRegistry == nil {
		return nil
	}
	h, err := s.jobsRegistry.Start(ctx, jobs.KindUpdate, "", updateMeta(release))
	if err != nil {
		return nil
	}
	return h
}

// openUpdateJobDetached is the async-202 sibling — handle outlives the
// HTTP request that accepted the update.
func (s *Scheduler) openUpdateJobDetached(release *ReleaseInfo) jobs.Handle {
	if s.jobsRegistry == nil {
		return nil
	}
	h, err := s.jobsRegistry.StartDetached(jobs.KindUpdate, "", updateMeta(release))
	if err != nil {
		return nil
	}
	return h
}

func updateMeta(release *ReleaseInfo) jobs.Meta {
	meta := jobs.Meta{"current_version": version.Current.String()}
	if release != nil {
		meta["target_version"] = release.Version.String()
	}
	return meta
}

// applyUpdate downloads, verifies, and installs an update. h is an
// optional jobs.Handle — when non-nil, every phase transition and
// download tick mirrors onto the SSE stream. Done/Fail are emitted by
// the caller (both ApplyNow and ApplyNowAsync go through this function).
func (s *Scheduler) applyUpdate(ctx context.Context, release *ReleaseInfo, h jobs.Handle) error {
	startTime := s.clock.Now()
	fromVersion := version.Current
	metrics := GetUpdateMetrics()

	// Detached signatures cannot authenticate publisher identity or signing time.
	bundleAsset := s.checker.GetBundleAsset(release)
	if bundleAsset == nil {
		err := fmt.Errorf("release requires checksums.txt.sigstore.json")
		s.recordUpdate(fromVersion, release.Version, false, err.Error(), s.clock.Since(startTime))
		return err
	}

	// Download
	s.setState(StateDownloading)
	if h != nil {
		h.Meta(jobs.Meta{"phase": "downloading", "target_version": release.Version.String()})
	}
	asset := s.checker.GetAssetForPlatform(release)
	if asset == nil {
		return fmt.Errorf("no asset found for current platform")
	}

	s.downloader.SetProgressCallback(func(downloaded, total int64, percent int) {
		s.mu.Lock()
		s.status.DownloadProgress = percent
		s.mu.Unlock()
		if h != nil {
			h.Progress(percent, "downloading", jobs.Bytes{Done: downloaded, Total: total})
		}
	})

	downloadRecorder := NewUpdateDownloadRecorder(ctx)
	downloadResult, err := s.downloader.Download(ctx, asset)
	if err != nil {
		downloadRecorder.RecordDownloadResult(UpdateStatusFailed, 0, false)
		s.recordUpdate(fromVersion, release.Version, false, err.Error(), s.clock.Since(startTime))
		return fmt.Errorf("download failed: %w", err)
	}
	downloadRecorder.RecordDownloadResult(UpdateStatusSuccess, downloadResult.Size, downloadResult.Resumed)

	// Download checksums
	checksumAsset := s.checker.GetChecksumAsset(release)
	_, err = s.downloader.DownloadChecksums(ctx, checksumAsset)
	if err != nil {
		s.recordUpdate(fromVersion, release.Version, false, err.Error(), s.clock.Since(startTime))
		return fmt.Errorf("checksums download failed: %w", err)
	}

	bundlePath, err := s.downloader.DownloadBundle(ctx, bundleAsset)
	if err != nil {
		s.recordUpdate(fromVersion, release.Version, false, err.Error(), s.clock.Since(startTime))
		return fmt.Errorf("signature bundle download failed: %w", err)
	}

	// Verify
	s.setState(StateVerifying)
	if h != nil {
		h.Progress(100, "verifying", jobs.Bytes{})
	}
	verifyResult, err := s.verifier.Verify(downloadResult, bundlePath, release.Version.String())
	if err != nil {
		s.recordUpdate(fromVersion, release.Version, false, err.Error(), s.clock.Since(startTime))
		return fmt.Errorf("verification failed: %w", err)
	}
	if !verifyResult.Valid {
		s.recordUpdate(fromVersion, release.Version, false, verifyResult.Error, s.clock.Since(startTime))
		return fmt.Errorf("verification failed: %s", verifyResult.Error)
	}

	// Install
	s.setState(StateApplying)
	if h != nil {
		h.Progress(100, "installing", jobs.Bytes{})
	}
	installRecorder := NewUpdateInstallRecorder(ctx)
	installResult, err := s.installer.Install(ctx, downloadResult.FilePath, release.Version)
	if err != nil {
		installRecorder.RecordInstallResult(UpdateStatusFailed, fromVersion.String(), release.Version.String())
		s.recordUpdate(fromVersion, release.Version, false, err.Error(), s.clock.Since(startTime))
		return fmt.Errorf("installation failed: %w", err)
	}
	if !installResult.Success {
		installRecorder.RecordInstallResult(UpdateStatusFailed, fromVersion.String(), release.Version.String())
		s.recordUpdate(fromVersion, release.Version, false, installResult.Error, s.clock.Since(startTime))
		return fmt.Errorf("installation failed: %s", installResult.Error)
	}
	installRecorder.RecordInstallResult(UpdateStatusSuccess, fromVersion.String(), release.Version.String())

	// Clear pending update metric
	if metrics != nil {
		metrics.SetPendingUpdate(ctx, false)
	}

	// Leave a note for the process that starts next. Only the boot after
	// this one can tell whether the binary just installed actually
	// serves, and only this one still knows what to go back to.
	if err := s.confirmer.Record(fromVersion, release.Version, installResult); err != nil {
		_, rollbackErr := s.installer.Rollback()
		return fmt.Errorf("record update confirmation: %w", errors.Join(err, rollbackErr))
	}
	if err := s.markOperationRestarting(); err != nil {
		_, rollbackErr := s.installer.Rollback()
		_ = s.confirmer.Commit()
		return fmt.Errorf("record update operation: %w", errors.Join(err, rollbackErr))
	}

	s.recordUpdate(fromVersion, release.Version, true, "", s.clock.Since(startTime))
	// Cleanup temp files
	_ = s.downloader.Cleanup()

	// Emit terminal BEFORE RestartService. The service manager may
	// SIGTERM/exec the process before SSE subscribers observe the final
	// event; flipping the order gives every subscriber a chance to see
	// phase=done before the TCP connection drops.
	if h != nil {
		h.Progress(100, "restarting", jobs.Bytes{})
		h.Done()
	}

	s.restartAfterApply()

	return nil
}

// restartAfterApply switches the running process over to the binary the
// install just wrote. The binary on disk is the new one either way —
// what is at stake is whether it is the code actually running, and a
// node that cannot restart itself says so in its status rather than
// reporting an update that silently did not take effect.
func (s *Scheduler) restartAfterApply() {
	if !s.config.IsAutoRestartEnabled() {
		s.setRestartRequired("auto_restart is disabled in node.yaml")
		slog.Info("update applied successfully - manual restart required")
		return
	}

	s.setState(StateRestarting)
	slog.Info("update applied successfully - handing over for the restart")

	if err := s.restarter.Restart(); err != nil {
		s.setRestartRequired(err.Error())
		slog.Warn("update applied but the node could not restart itself", "err", err)
	}
}

// setRestartRequired records that the new binary is on disk but the
// running process is still the old one. State goes back to idle: the
// update itself succeeded, and leaving it on "restarting" would claim a
// restart that is not coming.
func (s *Scheduler) setRestartRequired(reason string) {
	s.mu.Lock()
	s.status.State = StateIdle
	s.status.RestartRequired = true
	s.status.RestartRequiredReason = reason
	s.mu.Unlock()
}

// CheckNow triggers an immediate update check
func (s *Scheduler) CheckNow(ctx context.Context) (*CheckResult, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.setState(StateChecking)
	defer s.setState(StateIdle)

	checkRecorder := NewUpdateCheckRecorder(ctx)

	result, err := s.checker.Check(ctx)
	if err != nil {
		// Record check failure in metrics
		status := UpdateStatusFailed
		if _, ok := err.(*RateLimitError); ok {
			status = UpdateStatusRateLimited
		}
		checkRecorder.RecordCheckResult(status, false)
		return nil, err
	}

	s.setLastCheckTime(s.clock.Now())

	s.mu.Lock()
	if result.UpdateAvailable {
		s.status.UpdateAvailable = true
		s.status.LatestVersion = result.LatestRelease.Version
		s.status.PendingRelease = result.LatestRelease
	}
	s.mu.Unlock()

	checkRecorder.RecordCheckResult(UpdateStatusSuccess, result.UpdateAvailable)

	return result, nil
}

// ErrNoUpdateAvailable signals that the scheduler has no pending
// release to apply. HTTP handlers surface it as 409 Conflict (client
// precondition, not server fault).
var ErrNoUpdateAvailable = fmt.Errorf("no update available")

// ApplyNow applies a pending update immediately. Synchronous — returns
// only after the apply fully succeeds or fails. Mirrors progress onto
// an SSE stream if a jobs registry is wired. Panic in the apply is
// reported to the jobs.Handle before re-propagating so operator-
// observability (SSE) is not silently dropped.
func (s *Scheduler) ApplyNow(ctx context.Context) error {
	// Gate first — a redundant concurrent caller bails without doing a
	// network check.
	if !s.beginApply() {
		return ErrApplyInFlight
	}
	defer s.endApply()
	pending, err := s.resolvePending(ctx)
	if err != nil {
		return err
	}
	h := s.openUpdateJob(ctx, pending)
	defer func() {
		if r := recover(); r != nil {
			if h != nil {
				h.Fail(fmt.Errorf("apply panic: %v", r))
			}
			panic(r)
		}
	}()
	if err := s.applyUpdate(ctx, pending, h); err != nil {
		if h != nil {
			h.Fail(err)
		}
		return err
	}
	if h != nil {
		h.Done()
	}
	return nil
}

// ApplyNowAsync dispatches the apply in a detached goroutine tracked
// on Scheduler.wg so Stop drains it cleanly. Returns the jobs.Handle
// ID immediately (or "" when the registry is unwired) so the caller
// (HTTP handler or CLI) can hand it to the client for SSE subscription.
// The goroutine uses context.Background so client disconnect does not
// cancel the apply — updates must run to completion.
func (s *Scheduler) ApplyNowAsync(ctx context.Context) (string, error) {
	// Gate before resolvePending so a redundant caller doesn't burn a
	// CheckNow round-trip and also before openUpdateJob so the second
	// caller never sees a job_id for work that won't run.
	if !s.beginApply() {
		return "", ErrApplyInFlight
	}
	pending, err := s.resolvePending(ctx)
	if err != nil {
		s.endApply()
		return "", err
	}
	//nolint:contextcheck // the scheduler lifecycle owns the apply after HTTP acceptance
	return s.startResolvedApply(pending)
}

func (s *Scheduler) startResolvedApply(pending *ReleaseInfo) (string, error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		s.endApply()
		return "", context.Canceled
	}
	s.wg.Add(1)
	s.mu.Unlock()
	h := s.openUpdateJobDetached(pending)

	var jobID string
	if h != nil {
		jobID = h.ID()
	}
	var err error
	jobID, err = s.recordOperation(jobID, pending)
	if err != nil {
		s.wg.Done()
		s.endApply()
		if h != nil {
			h.Fail(err)
		}
		return "", err
	}

	// The scheduler owns this apply after the accepting request ends.
	go func() {
		defer s.wg.Done()
		defer s.endApply()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("update apply goroutine panicked",
					"panic", r, "stack", string(debug.Stack()), "job_id", jobID)
				err := fmt.Errorf("apply panic: %v", r)
				// The status is the only account of an apply for anyone
				// who is not subscribed to the job — and on a node with
				// no jobs registry there is no job at all. Leaving it on
				// the phase the panic interrupted reports work still in
				// progress that nothing is doing.
				s.setError(err)
				s.finishOperation(err)
				if h != nil {
					h.Fail(err)
				}
			}
		}()
		if err := s.applyUpdate(s.ctx, pending, h); err != nil {
			slog.Error("async update apply failed", "err", err, "job_id", jobID)
			// applyUpdate advances the state as it goes and leaves it
			// where it failed; only the caller turns that into failed.
			// The scheduled path does this (see checkAndApply); without
			// it here, a manual apply that fails pins the status at
			// `downloading` or `verifying` with an empty error, and a
			// caller watching the status waits forever.
			s.setError(err)
			s.finishOperation(err)
			if h != nil {
				h.Fail(err)
			}
			return
		}
		if h != nil {
			h.Done()
		}
	}()
	return jobID, nil
}

// resolvePending returns the release to apply. Uses cached pending if
// available; otherwise runs a fresh check.
func (s *Scheduler) resolvePending(ctx context.Context) (*ReleaseInfo, error) {
	s.mu.RLock()
	pending := s.status.PendingRelease
	s.mu.RUnlock()
	if pending != nil {
		return pending, nil
	}
	result, err := s.CheckNow(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	if !result.UpdateAvailable {
		return nil, ErrNoUpdateAvailable
	}
	return result.LatestRelease, nil
}

// Rollback rolls back to the previous version. The context is threaded
// to the metrics recorder so rollback spans join the caller's trace.
func (s *Scheduler) Rollback(ctx context.Context) (*InstallResult, error) {
	metrics := GetUpdateMetrics()
	result, err := s.installer.Rollback()

	if err != nil {
		if metrics != nil {
			metrics.RecordUpdateRollback(ctx, UpdateStatusFailed)
		}
		return result, err
	}

	if result != nil && result.Success {
		if metrics != nil {
			metrics.RecordUpdateRollback(ctx, UpdateStatusSuccess)
		}
		// The update this record was watching is no longer installed.
		// Leaving it would have the next boot count an attempt against a
		// version that is gone, and eventually "roll back" to the very
		// binary the operator just rolled away from.
		if err := s.confirmer.Commit(); err != nil {
			slog.Warn("rolled back but could not clear the pending update record", "err", err)
		}
	} else if metrics != nil {
		metrics.RecordUpdateRollback(ctx, UpdateStatusFailed)
	}

	return result, nil
}

// ConfirmUpdate accepts an update that has been proven to serve,
// clearing the record that would otherwise have the next boot count an
// attempt against it.
//
// Used by the privileged updater, which restarts the node and watches
// it come back within one process and so knows the answer immediately.
// The node's own boot-time confirmation is the same decision reached
// the long way round, for installs where nothing outlives the restart.
func (s *Scheduler) ConfirmUpdate() error { return s.confirmer.Commit() }

// GetStatus returns the current update status
func (s *Scheduler) GetStatus() UpdateStatus {
	s.mu.RLock()
	status := s.status
	s.mu.RUnlock()

	// Read from disk rather than memory: the record belongs to the boot,
	// and the process that wrote it is not this one.
	status.Enabled = s.Enabled()
	if pending, err := s.confirmer.load(); err == nil {
		status.PendingConfirm = pending
	} else {
		status.ConfirmationError = err.Error()
	}

	// On a delegated install the work happens in a process this one
	// cannot see, so the only honest source for "what happened" is what
	// that process published.
	if s.delegate != nil {
		status.DelegatedTo = s.delegate.RequestPath()
		if run, err := s.delegate.LastRun(); err == nil {
			status.PrivilegedRun = run
		} else {
			status.ConfirmationError = err.Error()
			slog.Warn("could not read what the privileged updater last did", "err", err)
		}
		status.Operation = status.PrivilegedRun
		if pending, err := s.delegate.Pending(); err != nil {
			status.ConfirmationError = err.Error()
		} else if pending != nil {
			status.Operation = &RunStatus{JobID: pending.JobID, Action: pending.Action, ToVersion: pending.TargetVersion, State: StateChecking}
		}
	} else {
		s.operationMu.Lock()
		var run RunStatus
		if err := readJSON(s.operationPath, &run); err == nil {
			status.Operation = &run
			if !run.Finished() && run.State != StateRestarting && !s.applying.Load() {
				finished := s.clock.Now().UTC()
				run.FinishedAt, run.State, run.Error = &finished, StateFailed, "node stopped before completing update"
				if err := writeJSONAtomic(s.operationPath, &run, 0600); err != nil {
					status.ConfirmationError = err.Error()
				}
			}
			if !run.Finished() && run.State == StateRestarting && status.PendingConfirm == nil && status.ConfirmationError == "" {
				finished := s.clock.Now().UTC()
				run.FinishedAt = &finished
				run.Success = MatchesVersion(status.CurrentVersion, run.ToVersion)
				if !run.Success {
					run.Error = "node returned on a different version after update"
				}
				if err := writeJSONAtomic(s.operationPath, &run, 0600); err != nil {
					status.ConfirmationError = err.Error()
				}
			}
		} else if !os.IsNotExist(err) {
			status.ConfirmationError = err.Error()
		}
		s.operationMu.Unlock()
	}
	return status
}

// GetHistory returns the update history
func (s *Scheduler) GetHistory() (*UpdateHistory, error) {
	return s.history.Load()
}

// isInMaintenanceWindow checks if we're currently in the maintenance window
func (s *Scheduler) isInMaintenanceWindow() bool {
	if !s.config.HasMaintenanceWindow() {
		return true // No window configured = always OK
	}

	// Parse cron expression
	// For simplicity, we support basic hour-based windows
	// Format: "0 3 * * *" = 3 AM daily
	// Full cron parsing would require a library like robfig/cron

	window := s.config.MaintenanceWindow
	now := s.clock.Now()

	// Simple parsing for hour-based windows (e.g., "0 3 * * *")
	var minute, hour int
	n, err := fmt.Sscanf(window, "%d %d * * *", &minute, &hour)
	if err != nil || n != 2 {
		// If we can't parse, default to allowing updates
		return true
	}

	// Check if current hour matches (with 1-hour window)
	currentHour := now.Hour()
	return currentHour == hour
}

// Helper methods for state management

func (s *Scheduler) setState(state UpdateState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = state
	if state != StateFailed {
		s.status.Error = ""
	}
}

func (s *Scheduler) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = StateFailed
	s.status.Error = err.Error()
}

func (s *Scheduler) setLastCheckTime(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastCheckTime = &t
}

func (s *Scheduler) setNextCheckTime(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.NextCheckTime = &t
}

func (s *Scheduler) recordUpdate(fromVersion, toVersion *version.Version, success bool, errorMsg string, duration time.Duration) {
	entry := UpdateHistoryEntry{
		ID:          fmt.Sprintf("%d", s.clock.Now().UnixNano()),
		Timestamp:   s.clock.Now(),
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Success:     success,
		Error:       errorMsg,
		Duration:    duration,
		Automatic:   true,
	}

	// Get backup path if available
	backups, _ := s.installer.ListBackups()
	if len(backups) > 0 {
		entry.BackupPath = backups[0]
	}

	if err := s.history.Add(entry); err != nil {
		slog.Warn("failed to record update history", "err", err)
	}

	if success {
		s.mu.Lock()
		now := s.clock.Now()
		s.status.LastUpdateTime = &now
		s.status.UpdateAvailable = false
		s.status.PendingRelease = nil
		s.mu.Unlock()
	}
}

// GetBackupDir returns the backup directory path
func (s *Scheduler) GetBackupDir() string {
	return s.backupDir
}

// EnsureDirectories ensures all required directories exist
func (s *Scheduler) EnsureDirectories() error {
	dirs := []string{s.tempDir, s.backupDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return nil
}

// Enabled reports whether scheduled automatic updates are enabled.
func (s *Scheduler) Enabled() bool { return s.enabled.Load() }

// SetEnabled changes scheduling; explicit manual updates remain available.
func (s *Scheduler) SetEnabled(enabled bool) { s.enabled.Store(enabled) }
