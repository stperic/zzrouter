package instance

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
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
//     SourceRepo, SizeBytes, Processor, ContextLength, Port, HealthURL,
//     Config, KeepAlive (initial), LogFilePath, LaunchCommand,
//     IdleExpiry, Logs.
//
//  2. **Mu-protected mutable** — written by Set/Mark/Stamp/Update
//     methods that take i.mu. Fields: Status, ErrorMessage, StartedAt,
//     FailedAt, LastHealthCheck, LastHealthy, LastActivity, KeepAlive
//     (after StampActivityWithHints), ProcessID, ProcessGroupID.
//
// Direct field writes from outside this package are restricted to
// bootstrap-only fields and only between NewInstance and Register.
// Use the dedicated setter (SetLogFilePath, SetProcessInfo, …) when
// the field has one — it documents the bootstrap-only contract at the
// call site and keeps the write surface auditable if locking is ever
// added.
type Instance struct {
	ID         string     `json:"id"`
	Provider   string     `json:"provider"`
	LaunchMode LaunchMode `json:"launch_mode"`
	Model      string     `json:"model,omitempty"`

	// StopUnconfirmed keeps a runtime occupied until its process reaper drains.
	StopUnconfirmed atomic.Bool `json:"-"`

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
	failure         *FailureInfo
	// StreamJobID is the pkg/jobs handle ID used by clients to watch
	// launch progress via /zzrouter/v1/jobs/:id/stream?node=<node>.
	// Empty when the instance predates jobs integration or was launched
	// without a jobs registry. Named to avoid collision with `jobHandle`
	// below (Windows process-group handle).
	StreamJobID   string         `json:"stream_job_id,omitempty"`
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

	// Concurrency semaphore: nil = unlimited, buffered chan = gated.
	// Initialized at most once via InitConcurrencyLimit (guarded by concurrencyOnce).
	concurrencyOnce sync.Once     `json:"-"`
	concurrencySem  chan struct{} `json:"-"`
	queueTimeout    time.Duration `json:"-"`

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

// IsRunning returns true if the instance is running or unhealthy.
func (i *Instance) IsRunning() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Status == StatusRunning || i.Status == StatusUnhealthy
}

// --- Thread-safe status transitions ---

// MarkStarting transitions to starting state.
func (i *Instance) MarkStarting() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Status = StatusStarting
	i.ErrorMessage = ""
	i.failure = nil
	i.FailedAt = time.Time{}
}

// MarkRunning transitions to running state.
func (i *Instance) MarkRunning() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Status.IsTerminal() || i.Status == StatusStopping {
		return
	}
	i.Status = StatusRunning
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
	i.Status = StatusUnhealthy
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
	i.Status = status
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

// snapshotKeepAlive reads KeepAlive and the most recent activity
// timestamp atomically under i.mu.RLock. Used by IsIdle / IsExpired /
// ShouldUnload so all three see a consistent view across a concurrent
// StampActivityWithHints write.
func (i *Instance) snapshotKeepAlive() (keepAlive time.Duration, activity time.Time) {
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
	i.resetKeepAliveTimerLocked()
}

// StampActivityWithHints applies any non-nil request hints, stamps activity
// timestamps (LastActivity and lastUsedAt), and resets the keep-alive timer
// — all under a single lock so IsIdle/IsExpired observers see a consistent
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

	now := utils.Now()
	i.LastActivity = now
	i.lastUsedAt = now
	i.resetKeepAliveTimerLocked()
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

// IsIdle returns true if the instance has been idle longer than keep-alive.
func (i *Instance) IsIdle() bool {
	keepAlive, activity := i.snapshotKeepAlive()
	if keepAlive <= 0 || activity.IsZero() {
		return false
	}
	return time.Since(activity) > keepAlive
}

// IsExpired returns true if the instance has exceeded its keep-alive duration.
func (i *Instance) IsExpired(now time.Time) bool {
	keepAlive, activity := i.snapshotKeepAlive()
	if keepAlive <= 0 || activity.IsZero() {
		return false
	}
	return now.After(activity.Add(keepAlive))
}

// ShouldUnload returns true if the instance should be unloaded. Uses one
// snapshot for the keep-alive read AND the activity comparison so the
// negative/zero/idle branches see a consistent view.
func (i *Instance) ShouldUnload() bool {
	keepAlive, activity := i.snapshotKeepAlive()
	if keepAlive < 0 {
		return false // indefinite
	}
	if keepAlive == 0 {
		return true // immediate
	}
	if activity.IsZero() {
		return false
	}
	return time.Since(activity) > keepAlive
}

func (i *Instance) resetKeepAliveTimerLocked() {
	if i.keepAliveTimer != nil {
		if !i.keepAliveTimer.Stop() {
			select {
			case <-i.keepAliveTimer.C:
			default:
			}
		}
	}

	if i.KeepAlive <= 0 {
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

// IncrementActiveRequests atomically increments the active request counter.
func (i *Instance) IncrementActiveRequests() int64 {
	return atomic.AddInt64(&i.activeRequests, 1)
}

// DecrementActiveRequests atomically decrements the active request counter.
func (i *Instance) DecrementActiveRequests() int64 {
	return atomic.AddInt64(&i.activeRequests, -1)
}

// GetActiveRequests returns the current number of active requests.
func (i *Instance) GetActiveRequests() int64 {
	return atomic.LoadInt64(&i.activeRequests)
}

// --- Concurrency gating ---

// InitConcurrencyLimit initializes the per-instance concurrency semaphore.
// max <= 0 means unlimited (no gating). A negative queueTimeout
// (constants.QueueUntilCallerDeadline) queues for as long as the caller
// waits. Thread-safe; subsequent calls are no-ops.
func (i *Instance) InitConcurrencyLimit(max int, queueTimeout time.Duration) {
	if max <= 0 {
		return
	}
	i.concurrencyOnce.Do(func() {
		i.concurrencySem = make(chan struct{}, max)
		i.queueTimeout = queueTimeout
	})
}

// AcquireConcurrency blocks until a concurrency slot is available, the context
// is cancelled, or the queue timeout expires. Returns nil on success.
// If the semaphore is nil (unlimited), returns immediately.
// On success, also increments the active request counter.
func (i *Instance) AcquireConcurrency(ctx context.Context) error {
	if i.concurrencySem == nil {
		i.IncrementActiveRequests()
		return nil
	}
	// Fast path: try non-blocking acquire first (avoids timer allocation)
	select {
	case i.concurrencySem <- struct{}{}:
		i.IncrementActiveRequests()
		return nil
	default:
	}
	// No slot available. A zero timeout rejects at the door; a negative one
	// holds the caller until its own context ends, which is the only bound
	// that matches what the caller will actually sit through.
	if i.queueTimeout == 0 {
		return ErrAtCapacity
	}
	if i.queueTimeout < 0 {
		select {
		case i.concurrencySem <- struct{}{}:
			i.IncrementActiveRequests()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	timer := time.NewTimer(i.queueTimeout)
	defer timer.Stop()
	select {
	case i.concurrencySem <- struct{}{}:
		i.IncrementActiveRequests()
		return nil
	case <-timer.C:
		return ErrAtCapacity
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseConcurrency releases a concurrency slot and decrements the active
// request counter. Must be called exactly once for each successful AcquireConcurrency.
func (i *Instance) ReleaseConcurrency() {
	if n := i.DecrementActiveRequests(); n < 0 {
		// Self-heal + log: a mismatched release is a programming error that
		// should not cause the idle reaper to see negative counts.
		atomic.StoreInt64(&i.activeRequests, 0)
		slog.Warn("ReleaseConcurrency called without matching Acquire", "instance", i.ID, "corrected_from", n)
	}
	if i.concurrencySem == nil {
		return
	}
	<-i.concurrencySem
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
		KeepAlive:             i.KeepAlive.String(),
		ErrorMessage:          i.ErrorMessage,
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
