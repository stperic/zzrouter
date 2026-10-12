package instance

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

const defaultLogBufferSize = 100

// Instance represents a running provider instance.
//
// LOCK ORDERING: Registry.mu → Instance.mu
// Instance.mu must NEVER be held when acquiring Registry.mu.
//
// # PUBLIC FIELD WRITE CONTRACT
//
// Exported fields fall into two classes:
//
//  1. **Bootstrap-only** — written by the constructor and the launching
//     code path BEFORE Registry.Register publishes the instance, then
//     treated as immutable. No lock needed because there is no
//     concurrent reader yet. Fields: ID, Provider, LaunchMode, Model,
//     SourceRepo, Port, HealthURL,
//     Config, KeepAlive (initial), LogFilePath, LaunchCommand,
//     IdleExpiry, Logs, FailureClassifier, StartedBy.
//
//  2. **Mu-protected mutable** — written by Set/Mark/Stamp/Update
//     methods that take i.mu. Fields: Status, ErrorMessage, StartedAt,
//     FailedAt, LastHealthCheck, LastHealthy, LastActivity, KeepAlive
//     (after StampActivityWithHints), ProcessID, ProcessGroupID,
//     SizeBytes, Processor, ContextLength (SetLoaded).
//
// Direct field writes from outside this package are restricted to
// bootstrap-only fields and only between NewInstance and Register.
// Use the dedicated setter (SetLogFilePath, SetProcessInfo, …) when
// the field has one — it documents the bootstrap-only contract at the
// call site and keeps the write surface auditable if locking is ever
// added.
type Instance struct {
	FailureClassifier *httperr.Classifier `json:"-"`
	ID                string              `json:"id"`
	Provider          string              `json:"provider"`
	LaunchMode        LaunchMode          `json:"launch_mode"`
	Model             string              `json:"model,omitempty"`
	weights           Weights

	// StopUnconfirmed keeps a runtime occupied until its process reaper drains.
	StopUnconfirmed atomic.Bool `json:"-"`
	// ending is held by the one stop or retirement ending the run; a failed stop gives it back.
	ending atomic.Bool

	// String not prov_apps.Endpoint to avoid a back-import (prov_apps imports instance).
	Endpoint   string `json:"endpoint,omitempty"`
	SourceRepo string `json:"source_repo,omitempty"`
	// WireModel is the token this engine keys on in the "model" field of
	// wire payloads: the canonical model name for most engines, the
	// weights path for engines that load by whatever the request carries.
	// The proxy uses it to speak the engine's dialect without leaking it
	// to clients.
	WireModel       string    `json:"wire_model,omitempty"`
	SizeBytes       int64     `json:"size_bytes,omitempty"`
	Processor       string    `json:"processor,omitempty"`
	ContextLength   int       `json:"context_length,omitempty"`
	ProcessID       int       `json:"process_id,omitempty"`
	ProcessGroupID  int       `json:"process_group_id,omitempty"`
	Port            int       `json:"port"`
	Status          Status    `json:"status"`
	HealthURL       string    `json:"health_url"`
	Config          Config    `json:"config"`
	StartedAt       time.Time `json:"started_at"`
	FailedAt        time.Time `json:"failed_at"`
	LastHealthCheck time.Time `json:"last_health_check"`
	LastHealthy     time.Time `json:"last_healthy"`
	LastActivity    time.Time `json:"last_activity"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	loaded          Loaded
	failure         *FailureInfo
	// StreamJobID is the pkg/jobs handle ID used by clients to watch
	// launch progress via /zzrouter/v1/jobs/:id/stream?node=<node>.
	// Empty when the instance predates jobs integration or was launched
	// without a jobs registry. Named to avoid collision with `jobHandle`
	// below (Windows process-group handle).
	StreamJobID   string         `json:"stream_job_id,omitempty"`
	StartedBy     StartedBy      `json:"started_by,omitempty"`
	KeepAlive     time.Duration  `json:"keep_alive"`
	LogFilePath   string         `json:"log_file_path,omitempty"`
	LaunchCommand *LaunchCommand `json:"launch_command,omitempty"`

	// resolved is what the launch actually computed after the full tier
	// walk: defaults -> model -> node -> node x model -> endpoint overlay
	// -> request. Config.Parameters holds only the REQUEST tier, which is
	// what a restart must replay.
	//
	// Both are kept because they answer different questions, and the
	// gap between them is where a whole bug class hid: a tier value
	// could persist, peer-sync and read back from /resolved while the
	// process never received it. It is also what a later config is
	// compared against to tell whether the run is still current.
	resolved Resolved

	// admitted is set once the run's claim on memory is accounted for; a
	// starting run that is not admitted is queued behind waitingOn.
	admitted     bool
	waitingOn    string
	evictedRuns  []EvictedRun
	failureCause error
	// stopClaimed/launchClaimed: which side signalled the process; a stop may retry its own claim.
	stopClaimed, launchClaimed bool
	// leftStarting closes when Status first leaves StatusStarting.
	leftStarting chan struct{}

	// Log channel for streaming instance output. Not serialized.
	Logs chan string `json:"-"`

	// Context for cancellation. Not serialized.
	ctx        context.Context    `json:"-"`
	cancelFunc context.CancelFunc `json:"-"`

	// mu protects mutable state: Status, ErrorMessage, timestamps, keep-alive state.
	// Use RLock for reads, Lock for writes.
	mu             sync.RWMutex `json:"-"`
	lastUsedAt     time.Time    `json:"-"`
	keepAliveTimer *time.Timer  `json:"-"`

	// Launcher wrapping flag (accessed atomically)
	usingLauncher atomic.Bool `json:"-"`

	// Log channel close synchronization
	closeLogsOnce sync.Once  `json:"-"`
	logsMu        sync.Mutex `json:"-"`
	logsClosed    bool       `json:"-"`

	// Goroutine tracking for graceful shutdown
	wg sync.WaitGroup `json:"-"`

	// Active request counter (atomic)
	activeRequests int64 `json:"-"`
	// Reservations keep later victims idle while earlier stops drain.
	idleStopClaimed bool `json:"-"`

	// Concurrency semaphore: nil = unlimited, buffered chan = gated.
	// Initialized at most once via InitConcurrencyLimit (guarded by concurrencyOnce).
	concurrencyOnce   sync.Once     `json:"-"`
	concurrencySem    chan struct{} `json:"-"`
	queueTimeout      time.Duration `json:"-"`
	queueMu           sync.Mutex    `json:"-"`
	waiters           int           `json:"-"`
	maxQueuedRequests int           `json:"-"`

	// IdleExpiry receives the instance ID when the keep-alive timer fires.
	// The manager listens on this to auto-stop idle instances.
	IdleExpiry chan string `json:"-"`

	// Windows Job Object handle. Zero on Unix or when not using Job Objects.
	// Stored as uintptr to avoid importing golang.org/x/sys/windows here.
	// The process package owns the lifecycle (create, assign, terminate, close).
	jobHandle uintptr `json:"-"`

	// Stdin pipe to the launcher. Closing it signals the launcher to initiate
	// graceful shutdown (stdin-EOF pattern). Nil on Unix.
	stdinPipe   io.WriteCloser `json:"-"`
	stdinClosed atomic.Bool    `json:"-"`
}

// NewInstance creates a new instance with the given configuration.
func NewInstance(id, provider, model string, port int, keepAlive time.Duration, logBufferSize int) *Instance {
	ctx, cancel := context.WithCancel(context.Background())

	if logBufferSize <= 0 {
		logBufferSize = defaultLogBufferSize
	}

	return &Instance{
		ID:         id,
		Provider:   provider,
		Model:      model,
		Port:       port,
		Status:     StatusStarting,
		KeepAlive:  keepAlive,
		LaunchMode: LaunchModeNative,
		Logs:       make(chan string, logBufferSize),
		ctx:        ctx,
		cancelFunc: cancel,
	}
}

// Context returns the instance's context (for cancellation propagation).
func (i *Instance) Context() context.Context {
	return i.ctx
}

// Cancel cancels the instance's context.
func (i *Instance) Cancel() {
	if i.cancelFunc != nil {
		i.cancelFunc()
	}
}

// ClaimEnd takes the right to end the run; false while another stop or retirement holds it.
func (i *Instance) ClaimEnd() bool { return i.ending.CompareAndSwap(false, true) }

// ReleaseEnd gives the right back after a stop that failed, so a retry can take it.
func (i *Instance) ReleaseEnd() { i.ending.Store(false) }

// --- Thread-safe status reads ---

// GetStatus returns the current status.
func (i *Instance) GetStatus() Status {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Status
}

// GetErrorMessage returns the current error message.
func (i *Instance) GetErrorMessage() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.ErrorMessage
}

// GetStartedAt returns when this instance began starting, zero if it has
// not been stamped yet. StartedAt is guarded by i.mu (see the type's
// concurrency note), so readers outside the package go through here.
func (i *Instance) GetStartedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.StartedAt
}

// IsHealthy returns true if the instance is running.
func (i *Instance) IsHealthy() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Status == StatusRunning
}

// Evictable reports whether a running or unhealthy run has no active leases and no stop claimed.
func (i *Instance) Evictable() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.evictableLocked()
}

func (i *Instance) evictableLocked() bool {
	return (i.Status == StatusRunning || i.Status == StatusUnhealthy) && i.GetActiveRequests() == 0 && !i.idleStopClaimed
}

// IsRunning returns true if the instance is running or unhealthy.
func (i *Instance) IsRunning() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Status == StatusRunning || i.Status == StatusUnhealthy
}

// --- Thread-safe status transitions ---

// setStatusLocked is the only writer of Status; callers hold i.mu.
func (i *Instance) setStatusLocked(status Status) {
	if i.Status == StatusStarting && status != StatusStarting && i.leftStarting != nil {
		select {
		case <-i.leftStarting: // SetStatus can put a run back to starting; close once
		default:
			close(i.leftStarting)
		}
	}
	i.Status = status
}

// LeftStarting is closed once the run is no longer starting.
func (i *Instance) LeftStarting() <-chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.leftStarting == nil {
		i.leftStarting = make(chan struct{})
		if i.Status != StatusStarting {
			close(i.leftStarting)
		}
	}
	return i.leftStarting
}

// Admit records that a starting run's memory claim is accounted for. It
// returns false when the run stopped while it waited.
func (i *Instance) Admit() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Status != StatusStarting {
		return false
	}
	i.admitted, i.waitingOn = true, ""
	return true
}

// MarkFailedError retains the typed cause before publishing the failed state.
func (i *Instance) MarkFailedError(err error) {
	i.markFailedWithExit(err.Error(), err, nil, "")
}

// FailureCause returns the typed error retained by a failed run.
func (i *Instance) FailureCause() error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.failureCause
}

// Admitted reports whether the run's memory claim is accounted for.
func (i *Instance) Admitted() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.admitted
}

// SetWaitingOn names the starting run this queued run waits for.
func (i *Instance) SetWaitingOn(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.waitingOn = id
}

// WaitingOn returns the run a queued run waits for, if known.
func (i *Instance) WaitingOn() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.waitingOn
}

// SetEvictedRuns records the idle runs this launch stopped to be admitted.
func (i *Instance) SetEvictedRuns(runs []EvictedRun) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.evictedRuns = slices.Clone(runs)
}

// EvictedRuns returns the idle runs this launch stopped to be admitted.
func (i *Instance) EvictedRuns() []EvictedRun {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return slices.Clone(i.evictedRuns)
}

// MarkRunning transitions to running state.
func (i *Instance) MarkRunning() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Status.IsTerminal() || i.Status == StatusStopping {
		return
	}
	i.setStatusLocked(StatusRunning)
	i.LastHealthy = utils.Now()
	i.ErrorMessage = ""
	i.failure = nil
}

// MarkUnhealthy transitions to unhealthy state with a reason.
func (i *Instance) MarkUnhealthy(errorMsg string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Status.IsTerminal() || i.Status == StatusStopping {
		return
	}
	i.setStatusLocked(StatusUnhealthy)
	i.ErrorMessage = errorMsg
}

// MarkFailed transitions to failed state with a reason.
func (i *Instance) MarkFailed(errorMsg string) {
	i.MarkFailedWithExit(errorMsg, nil, "")
}

// SetStatus sets the status directly for non-standard transitions.
func (i *Instance) SetStatus(status Status) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setStatusLocked(status)
}

// --- Process info (set by launcher) ---

// SetProcessInfo atomically sets all process-related fields after launch.
// This prevents data races with concurrent readers (health monitor, ToInfo).
// SetResolved records what the launch resolved, so the API can report a
// launch fact rather than only a config read.
func (i *Instance) SetResolved(r Resolved) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.resolved = r.clone()
}

// Resolved returns a copy of what the launch resolved.
func (i *Instance) Resolved() Resolved {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.resolved.clone()
}

func (i *Instance) SetProcessInfo(pid, pgid int, cmd *LaunchCommand) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ProcessID = pid
	i.ProcessGroupID = pgid
	i.LaunchCommand = cmd
}

// SetLogFilePath stamps the on-disk log path. Bootstrap-only: must be
// called before Registry.Register publishes the instance, so no lock
// is needed and no concurrent reader can observe a torn write. The
// dedicated method exists so the contract is explicit at every call
// site (manager_lifecycle and integration test setup) — direct field
// assignment would silently break if locking were added later.
func (i *Instance) SetLogFilePath(path string) {
	i.LogFilePath = path
}

// ClaimTermination returns the recorded pid when the caller is to signal it,
// else 0. Whichever of a stop and the launching goroutine claims first owns
// the signal; only a stop may claim again, to retry its own failed stop.
func (i *Instance) ClaimTermination(byStop bool) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ProcessID <= 0 || i.launchClaimed || (i.stopClaimed && !byStop) {
		return 0
	}
	i.stopClaimed, i.launchClaimed = byStop, !byStop
	return i.ProcessID
}

// GetProcessID returns the process ID under lock.
func (i *Instance) GetProcessID() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.ProcessID
}

// GetProcessGroupID returns the process group ID under lock.
func (i *Instance) GetProcessGroupID() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.ProcessGroupID
}

// --- Timestamp accessors ---

// SetLastHealthCheck records a health check time.
func (i *Instance) SetLastHealthCheck(t time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.LastHealthCheck = t
}

// SetStartedAtIfZero sets StartedAt to now if not already set.
func (i *Instance) SetStartedAtIfZero() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.StartedAt.IsZero() {
		i.StartedAt = utils.Now()
	}
}

// lastActivityTime returns the most recent activity timestamp.
// Prefers LastActivity (set by external tracking) over lastUsedAt (set by keep-alive).
// Caller must hold at least i.mu.RLock.
func (i *Instance) lastActivityTime() time.Time {
	if !i.LastActivity.IsZero() {
		return i.LastActivity
	}
	return i.lastUsedAt
}

// IdleSnapshot reads keep-alive and last activity under one lock, the inputs of the idle rule.
func (i *Instance) IdleSnapshot() (keepAlive time.Duration, activity time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.KeepAlive, i.lastActivityTime()
}

// GetLastActivity returns the last activity time.
func (i *Instance) GetLastActivity() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.lastActivityTime()
}

// Loaded is what the daemon serving a run reports of its model.
type Loaded struct {
	Size, VRAM                                        int64
	ContextLength                                     int
	ExpiresAt                                         time.Time // when the daemon unloads it idle
	Digest, Format, Family, ParameterSize, QuantLevel string
}

// percentWhole is a whole share in percent.
const percentWhole = 100

// SetLoaded records, for listings, what the daemon serving the run reports of its model.
func (i *Instance) SetLoaded(l Loaded) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.loaded, i.SizeBytes, i.ContextLength = l, l.Size, l.ContextLength
	switch {
	case l.Size <= 0:
		i.Processor = ""
	case l.VRAM >= l.Size:
		i.Processor = "100% GPU"
	case l.VRAM <= 0:
		i.Processor = "100% CPU"
	default:
		gpu := float64(l.VRAM) / float64(l.Size) * percentWhole
		i.Processor = fmt.Sprintf("%.0f%% GPU / %.0f%% CPU", gpu, percentWhole-gpu)
	}
}

// Loaded returns what SetLoaded recorded.
func (i *Instance) Loaded() Loaded {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.loaded
}

// --- Keep-alive ---

// GetKeepAlive returns the current keep-alive duration. Safe to read
// concurrently with StampActivityWithHints, which mutates the field under
// i.mu.
func (i *Instance) GetKeepAlive() time.Duration {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.KeepAlive
}

// UpdateLastUsed updates the last-used timestamp and resets the keep-alive timer.
func (i *Instance) UpdateLastUsed() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.lastUsedAt = utils.Now()
	i.resetKeepAliveTimerLocked(false)
}

// StampActivityWithHints applies any non-nil request hints, stamps activity
// timestamps (LastActivity and lastUsedAt), and resets the keep-alive timer
// — all under a single lock so idle observers see a consistent
// view.
//
// ov may be nil (no overrides). Individual Override fields may be nil; only
// the ones that are set take effect.
//
// Concurrency: last-one-wins. If two requests with different KeepAlive
// values race, the last acquire of the lock determines the timer's new
// deadline. The callers' own activity timestamps are both honored because
// resetKeepAliveTimerLocked always fires fresh.
func (i *Instance) StampActivityWithHints(ov *keepalive.Override) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if ov != nil {
		if ov.Duration != nil {
			i.KeepAlive = *ov.Duration
		}
	}

	i.stampLocked(false)
}

// released drops one hold under the idle rule's lock, so the reaper never sees the count without the re-armed timer.
func (i *Instance) released() {
	i.mu.Lock()
	defer i.mu.Unlock()
	atomic.AddInt64(&i.activeRequests, -1)
	i.stampLocked(true)
}

func (i *Instance) stampLocked(release bool) {
	now := utils.Now()
	i.LastActivity = now
	i.lastUsedAt = now
	i.resetKeepAliveTimerLocked(release)
}

// GetLastUsedAt returns the last-used timestamp.
func (i *Instance) GetLastUsedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.lastUsedAt
}

// StopKeepAliveTimer stops the keep-alive timer.
func (i *Instance) StopKeepAliveTimer() {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.keepAliveTimer != nil {
		if !i.keepAliveTimer.Stop() {
			select {
			case <-i.keepAliveTimer.C:
			default:
			}
		}
		i.keepAliveTimer = nil
	}
}

// resetKeepAliveTimerLocked arms expiry: below zero never, zero only as a request releases the run.
func (i *Instance) resetKeepAliveTimerLocked(release bool) {
	if i.keepAliveTimer != nil {
		if !i.keepAliveTimer.Stop() {
			select {
			case <-i.keepAliveTimer.C:
			default:
			}
		}
	}

	// A run being stopped arms nothing: its stop has already cleared the timer.
	if i.KeepAlive < 0 || (i.KeepAlive == 0 && !release) || (i.ctx != nil && i.ctx.Err() != nil) {
		i.keepAliveTimer = nil
		return
	}

	keepAlive := i.KeepAlive
	id := i.ID
	i.keepAliveTimer = time.AfterFunc(keepAlive, func() {
		i.TryLog(fmt.Sprintf("Keep-alive expired after %v, scheduling unload", keepAlive))
		if i.IdleExpiry != nil {
			select {
			case i.IdleExpiry <- id:
			default:
			}
		}
	})
}

// --- Active request tracking (atomic) ---

// GetActiveRequests returns the current number of active requests.
func (i *Instance) GetActiveRequests() int64 {
	return atomic.LoadInt64(&i.activeRequests)
}

// --- Concurrency gating ---

// ConcurrencyConfig is an instance's admission policy. A zero MaxConcurrentRequests means
// unlimited; a negative QueueTimeout waits as long as the caller does.
type ConcurrencyConfig struct {
	MaxConcurrentRequests int
	MaxQueuedRequests     int // 0 = never queue: refuse whenever no slot is free
	QueueTimeout          time.Duration
}

// InitConcurrencyLimit sets the admission policy once; later calls are no-ops.
func (i *Instance) InitConcurrencyLimit(cfg ConcurrencyConfig) {
	if cfg.MaxConcurrentRequests <= 0 {
		return
	}
	i.concurrencyOnce.Do(func() {
		i.concurrencySem = make(chan struct{}, cfg.MaxConcurrentRequests)
		i.queueTimeout = cfg.QueueTimeout
		i.maxQueuedRequests = cfg.MaxQueuedRequests
	})
}

// ConcurrencyLimit returns the max concurrent requests (0 = unlimited).
func (i *Instance) ConcurrencyLimit() int {
	if i.concurrencySem == nil {
		return 0
	}
	return cap(i.concurrencySem)
}

// --- Goroutine tracking ---

// TrackGoroutine increments the WaitGroup before spawning a tracked goroutine.
func (i *Instance) TrackGoroutine() {
	i.wg.Add(1)
}

// GoroutineDone decrements the WaitGroup when a tracked goroutine exits.
func (i *Instance) GoroutineDone() {
	i.wg.Done()
}

// WaitForGoroutines blocks until all tracked goroutines exit or ctx is
// cancelled. Returns nil on clean drain, or ctx.Err() if the deadline fires
// before goroutines exit — in which case the goroutines are effectively
// leaked and the caller must decide whether to continue cleanup best-effort.
func (i *Instance) WaitForGoroutines(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		i.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- Log channel ---

// TryLog sends a log message without blocking. Drops the message if the channel is full or closed.
func (i *Instance) TryLog(message string) {
	if i.Logs == nil {
		return
	}

	i.logsMu.Lock()
	defer i.logsMu.Unlock()

	if i.logsClosed {
		return
	}

	select {
	case i.Logs <- message:
	default:
	}
}

// CloseLogs closes the log channel exactly once.
func (i *Instance) CloseLogs() {
	i.closeLogsOnce.Do(func() {
		i.logsMu.Lock()
		defer i.logsMu.Unlock()
		i.logsClosed = true
		close(i.Logs)
	})
}

// --- Launcher flag ---

// SetUsingLauncher marks the instance as launched via zzrouter-launcher.
func (i *Instance) SetUsingLauncher(v bool) {
	i.usingLauncher.Store(v)
}

// IsUsingLauncher returns whether the instance uses the launcher wrapper.
func (i *Instance) IsUsingLauncher() bool {
	return i.usingLauncher.Load()
}

// --- Job Object / Stdin Pipe (Windows process lifecycle) ---

// SetJobHandle stores the Windows Job Object handle for this instance.
func (i *Instance) SetJobHandle(h uintptr) { i.jobHandle = h }

// GetJobHandle returns the stored Job Object handle (0 if none).
func (i *Instance) GetJobHandle() uintptr { return i.jobHandle }

// SetStdinPipe stores the write end of the launcher's stdin pipe.
func (i *Instance) SetStdinPipe(w io.WriteCloser) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.stdinPipe = w
}

// CloseStdinPipe closes the launcher's stdin pipe, signaling it to begin
// graceful shutdown. Idempotent — safe to call multiple times.
func (i *Instance) CloseStdinPipe() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stdinPipe != nil && i.stdinClosed.CompareAndSwap(false, true) {
		_ = i.stdinPipe.Close()
	}
}

// HasStdinPipe returns true if a stdin pipe is configured (Windows path).
func (i *Instance) HasStdinPipe() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.stdinPipe != nil
}

// --- Snapshot ---

// ToInfo creates a read-only InstanceInfo snapshot for API responses.
// hostname is the externally-reachable name of this node; it replaces the
// localhost in the stored HealthURL so coordinators (and any cross-node
// caller) get a URL they can actually dial.
func (i *Instance) ToInfo(hostname string) InstanceInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()

	info := InstanceInfo{
		Runtime: i.Config.Runtime, DisposablePlanID: i.Config.DisposablePlanID,
		ID:                    i.ID,
		Provider:              i.Provider,
		LaunchMode:            string(i.LaunchMode),
		Model:                 i.Model,
		Endpoint:              i.Endpoint,
		SourceRepo:            i.SourceRepo,
		SizeBytes:             i.SizeBytes,
		Processor:             i.Processor,
		ContextLength:         i.ContextLength,
		ProcessID:             i.ProcessID,
		Port:                  i.Port,
		Status:                i.Status,
		HealthURL:             rewriteHealthURLHost(i.HealthURL, hostname),
		Node:                  hostname,
		KeepAlive:             FormatKeepAlive(i.KeepAlive),
		ErrorMessage:          i.ErrorMessage,
		WaitingOn:             i.waitingOn,
		EvictedRuns:           slices.Clone(i.evictedRuns),
		StartedBy:             i.StartedBy,
		Failure:               cloneFailure(i.failure),
		ActiveRequests:        atomic.LoadInt64(&i.activeRequests),
		MaxConcurrentRequests: i.ConcurrencyLimit(),
		Parameters:            copyStringMap(i.Config.Parameters),
		ResolvedParameters:    copyStringMap(i.resolved.Parameters),
		Environment:           copyStringMap(i.Config.EnvVars),
	}
	if i.resolved.Runtime != "" {
		info.Runtime = i.resolved.Runtime
	}

	if !i.StartedAt.IsZero() {
		info.StartedAt = i.StartedAt.Format(time.RFC3339)
		info.Uptime = time.Since(i.StartedAt).Truncate(time.Second).String()
	}
	if !i.LastActivity.IsZero() {
		info.LastActivity = i.LastActivity.Format(time.RFC3339)
	}
	if !i.LastHealthCheck.IsZero() {
		info.LastHealthCheck = i.LastHealthCheck.Format(time.RFC3339)
	}
	if !i.loaded.ExpiresAt.IsZero() {
		info.ExpiresAt = i.loaded.ExpiresAt.Format(time.RFC3339)
	}

	return info
}

// SnapshotConfig returns a deep copy of the launch Config with maps cloned,
// taken under the instance read lock. Use this when you need to read launch
// parameters from outside the instance package without racing with writers.
func (i *Instance) SnapshotConfig() Config {
	i.mu.RLock()
	defer i.mu.RUnlock()
	cfg := i.Config
	cfg.Parameters = copyStringMap(i.Config.Parameters)
	cfg.EnvVars = copyStringMap(i.Config.EnvVars)
	return cfg
}

// rewriteHealthURLHost replaces the host portion of a stored HealthURL with
// the externally-reachable hostname. The launcher records HealthURL as
// http://localhost:<port>/<path> because it can't know what name remote
// callers will use; this rewrites it at marshal time so coordinators get a
// dialable URL. Empty hostname or empty/invalid input returns the input
// unchanged. Non-loopback hosts are preserved (they were set deliberately —
// note: mDNS `.local` names are NOT loopback and are left alone).
func rewriteHealthURLHost(healthURL, hostname string) string {
	if healthURL == "" || hostname == "" {
		return healthURL
	}
	u, err := url.Parse(healthURL)
	if err != nil || u.Host == "" {
		return healthURL
	}
	host := u.Hostname()
	isLoopback := host == constants.Localhost || net.ParseIP(host).IsLoopback()
	if !isLoopback {
		return healthURL
	}
	if port := u.Port(); port != "" {
		u.Host = hostname + ":" + port
	} else {
		u.Host = hostname
	}
	return u.String()
}

// copyStringMap returns a shallow copy of a string map, or nil if the input is nil.
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	maps.Copy(cp, m)
	return cp
}

// FormatKeepAlive writes a keep-alive as its parameter reads: -1 for never.
func FormatKeepAlive(d time.Duration) string {
	if d < 0 {
		return "-1"
	}
	return d.String()
}

// Weights is a run's physical reservation, separate from its addressed name.
type Weights struct {
	Model, Source, Backend string
	Paths                  []string
}

// SetWeights replaces a queued reservation while the manager holds catalog permission.
func (i *Instance) SetWeights(weights Weights) {
	i.mu.Lock()
	defer i.mu.Unlock()
	weights.Paths = slices.Clone(weights.Paths)
	i.weights = weights
}

// Weights returns the physical reservation without exposing mutable state.
func (i *Instance) Weights() Weights {
	i.mu.RLock()
	defer i.mu.RUnlock()
	weights := i.weights
	weights.Paths = slices.Clone(weights.Paths)
	return weights
}
