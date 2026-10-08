package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/jobs"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// maxConcurrentSyncOps bounds parallel /sync/manifest + /sync/file
// operations across the executor. Kept small: saturating file reads
// are already enough to keep a worker busy.
const maxConcurrentSyncOps = 4

// allowedSyncFormats is the whitelist of accepted model formats for sync operations
var allowedSyncFormats = map[string]bool{"gguf": true, "safetensors": true, "mlx": true, "bin": true, "pt": true}

// SyncExecutor handles internal model sync operations over the mTLS
// cluster-port listener. Worker pulls its mTLS dispatch client from
// clusternode.Node and rewrites the coordinator-supplied source URL
// onto the cluster port before dialing.
type SyncExecutor struct {
	mtlsClient     func() *http.Client // Required on workers; nil on non-workers refuses sync.
	clusterPort    int                 // Worker's cluster port — used to rewrite sourceURL.
	shutdownCtx    func() context.Context
	refreshIndex   func() error
	getEndpoints   func() []*mesh.Endpoint
	jobs           *jobs.Registry // optional; when wired, sync progress fans out on KindSync stream
	security       *modelsync.Security
	ensureFeatures func(context.Context, string, string, []string, []string, func(string)) error

	// Global concurrency cap for /sync/manifest + /sync/file. A
	// buffered channel is strictly cheaper than x/sync/semaphore for
	// non-blocking acquire.
	sem chan struct{}

	// In-flight /sync/deploy dedup. singleflight doesn't fit: callers
	// need 409 + existing-job-id, not to block on the first request.
	inflightMu sync.Mutex
	inflight   map[string]string // "model|format" → job ID
}

// NewSyncExecutor creates a new sync executor. mtlsClient must return a
// Worker-mode mTLS client (from clusternode.Node.DialClient); a nil
// return refuses the sync request. clusterPort is the worker's own
// bind port — sourceURLs from the coordinator carry the peer's public
// port and must be rewritten onto the cluster port before dispatch.
func NewSyncExecutor(
	mtlsClient func() *http.Client,
	clusterPort int,
	shutdownCtx func() context.Context,
	refreshIndex func() error,
	getEndpoints func() []*mesh.Endpoint,
) *SyncExecutor {
	return &SyncExecutor{
		mtlsClient:   mtlsClient,
		clusterPort:  clusterPort,
		shutdownCtx:  shutdownCtx,
		refreshIndex: refreshIndex,
		getEndpoints: getEndpoints,
		security:     modelsync.NewSecurity(),
		sem:          make(chan struct{}, maxConcurrentSyncOps),
		inflight:     map[string]string{},
	}
}

// WithJobs wires a jobs.Registry so /sync/deploy emits KindSync events
// on the jobs stream. Safe to skip — when nil the sync runs without SSE
// integration (producer falls back to slog only).
func (e *SyncExecutor) WithJobs(r *jobs.Registry) *SyncExecutor {
	e.jobs = r
	return e
}

// acquireSlot tries to take a concurrency slot without blocking. Returns
// true on success; caller must releaseSlot exactly once.
func (e *SyncExecutor) acquireSlot() bool {
	select {
	case e.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (e *SyncExecutor) releaseSlot() { <-e.sem }

// withSlot acquires a concurrency slot, runs fn, and releases the slot.
// Writes 429 + Retry-After and skips fn when the semaphore is full.
func (e *SyncExecutor) withSlot(c *gin.Context, fn func()) {
	if !e.acquireSlot() {
		c.Header("Retry-After", "5")
		RespondWithProblem(c, http.StatusTooManyRequests, "Too Many Requests", "sync executor is at capacity; retry shortly")
		return
	}
	defer e.releaseSlot()
	fn()
}

// claimInflightDeploy reserves key for the given job, or returns the
// existing job ID and false if another deploy already holds it.
// Successful claims MUST be paired with releaseInflightDeploy via defer.
func (e *SyncExecutor) claimInflightDeploy(key, jobID string) (string, bool) {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	if existing, ok := e.inflight[key]; ok {
		return existing, false
	}
	e.inflight[key] = jobID
	return "", true
}

func (e *SyncExecutor) releaseInflightDeploy(key string) {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	delete(e.inflight, key)
}

// HandleGetManifest returns a manifest of files for a model
// GET /zzrouter/internal/sync/manifest?model=org/model&format=gguf
func (e *SyncExecutor) HandleGetManifest(c *gin.Context) {
	e.withSlot(c, func() { e.handleGetManifest(c) })
}

func (e *SyncExecutor) handleGetManifest(c *gin.Context) {
	model := c.Query("model")
	format := c.Query("format")
	request := metadata.DownloadRequest{Repo: model}
	if c.Request.Method == http.MethodPost {
		if !BindJSON(c, &request) {
			return
		}
		if request.Repo != model {
			BadRequest(c, "file plan does not match model")
			return
		}
	}

	if model == "" {
		BadRequest(c, "Specify the model ID (e.g., model=meta-llama/Llama-3-8B)")
		return
	}

	// Default format to gguf if not specified
	if format == "" {
		format = "gguf"
	}

	// Validate format against whitelist
	if !allowedSyncFormats[format] {
		BadRequest(c, fmt.Sprintf("Format %q is not supported. Allowed formats: gguf, safetensors, mlx, bin, pt", format))
		return
	}

	slog.Info("[SyncExecutor] GetManifest", "model", model, "format", format)

	// Build path to model directory
	// Models are stored as: {models_root}/{org}/{model}/{format}/
	// Generate manifest with checksums
	manifest, err := e.security.GenerateManifest(request, format)
	if err != nil {
		slog.Error("[SyncExecutor] GetManifest error", "error", err)

		if errors.Is(err, fs.ErrNotExist) {
			NotFound(c, fmt.Sprintf("model %s with format %s not found on this node", model, format))
			return
		}

		InternalNodeError(c, "Failed to generate manifest")
		return
	}

	slog.Info("[SyncExecutor] GetManifest: generated manifest with files, total size: bytes", "count", len(manifest.Files), "size", manifest.TotalSize)

	c.JSON(http.StatusOK, manifest)
}

// HandleDownloadFile streams a model file to the requesting node
// GET /zzrouter/internal/sync/file?model=org/model&format=gguf&file=model.gguf
// Supports Range requests for resumable downloads
func (e *SyncExecutor) HandleDownloadFile(c *gin.Context) {
	e.withSlot(c, func() { e.handleDownloadFile(c) })
}

func (e *SyncExecutor) handleDownloadFile(c *gin.Context) {
	model := c.Query("model")
	format := c.Query("format")
	fileName := c.Query("file")

	if model == "" || fileName == "" {
		BadRequest(c, "Specify model=org/model and file=filename.gguf")
		return
	}

	if format == "" {
		format = "gguf"
	}

	// Validate format against whitelist
	if !allowedSyncFormats[format] {
		BadRequest(c, fmt.Sprintf("Format %q is not supported. Allowed formats: gguf, safetensors, mlx, bin, pt", format))
		return
	}

	slog.Info("[SyncExecutor] DownloadFile", "model", model, "format", format, "file", fileName)

	// Build and validate the full file path
	filePath := filepath.Join(model, fileName)
	validPath, err := e.security.ValidateModelFile(filePath)
	if err != nil {
		slog.Error("[SyncExecutor] DownloadFile validation error", "error", err)

		if errors.Is(err, fs.ErrNotExist) {
			NotFound(c, fmt.Sprintf("file %s not found for model %s", fileName, model))
			return
		}

		BadRequest(c, "invalid file path")
		return
	}

	// Open the file
	file, err := os.Open(validPath)
	if err != nil {
		slog.Error("[SyncExecutor] DownloadFile open error", "error", err)
		InternalNodeError(c, "Failed to open file")
		return
	}
	defer func() { _ = file.Close() }()

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		InternalNodeError(c, "Failed to stat file")
		return
	}

	fileSize := fileInfo.Size()

	// Handle Range requests for resumable downloads
	rangeHeader := c.GetHeader("Range")
	if rangeHeader != "" {
		e.handleRangeRequest(c, file, fileSize, rangeHeader, fileName)
		return
	}

	// Full file download
	slog.Info("[SyncExecutor] DownloadFile: streaming full file ( bytes)", "file_size", fileSize)

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", strconv.FormatInt(fileSize, 10))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	c.Header("Accept-Ranges", "bytes")

	// Stream the file
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, file)
}

// handleRangeRequest handles HTTP Range requests for partial downloads
func (e *SyncExecutor) handleRangeRequest(c *gin.Context, file *os.File, fileSize int64, rangeHeader, fileName string) {
	// Parse Range header: "bytes=start-end" or "bytes=start-"
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		BadRequest(c, "Range header must start with 'bytes='")
		return
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		BadRequest(c, "Range format should be 'bytes=start-end' or 'bytes=start-'")
		return
	}

	var start, end int64
	var err error

	// Parse start
	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			BadRequest(c, "Start byte must be a non-negative integer")
			return
		}
	}

	// Parse end (or default to end of file)
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			BadRequest(c, "End byte must be >= start byte")
			return
		}
	} else {
		end = fileSize - 1
	}

	// Validate range
	if start >= fileSize {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", fileSize))
		c.Status(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	if end >= fileSize {
		end = fileSize - 1
	}

	contentLength := end - start + 1

	slog.Info("[SyncExecutor] DownloadFile: Range request bytes=- (length=)", "bytes", start, "end", end, "length", contentLength)

	// Seek to start position
	_, err = file.Seek(start, io.SeekStart)
	if err != nil {
		InternalNodeError(c, "Failed to seek file")
		return
	}

	// Send partial content response
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	c.Header("Accept-Ranges", "bytes")

	c.Status(http.StatusPartialContent)

	// Stream the requested range
	_, _ = io.CopyN(c.Writer, file, contentLength)
}

// HandleCheckModelExists checks if a model exists on this node
// GET /zzrouter/internal/sync/exists?model=org/model&format=gguf
func (e *SyncExecutor) HandleCheckModelExists(c *gin.Context) {
	model := c.Query("model")
	format := c.Query("format")
	if c.Request.Method == http.MethodPost {
		var req metadata.DownloadRequest
		if !BindJSON(c, &req) {
			return
		}
		if req.Repo != model {
			BadRequest(c, "file plan does not match model")
			return
		}
		held, err := e.security.Holds(req)
		c.JSON(http.StatusOK, gin.H{"exists": err == nil && held, "model": model, "format": format})
		return
	}

	if model == "" {
		BadRequest(c, "Specify the model ID (e.g., model=meta-llama/Llama-3-8B)")
		return
	}

	if format == "" {
		format = "gguf"
	}

	// Validate format against whitelist
	if !allowedSyncFormats[format] {
		BadRequest(c, fmt.Sprintf("Format %q is not supported. Allowed formats: gguf, safetensors, mlx, bin, pt", format))
		return
	}

	// Build path to model directory
	modelPath := model
	modelsRoot, err := modelregistry.GetModelsRootDir()
	if err != nil {
		InternalNodeError(c, "Failed to get models directory")
		return
	}
	fullPath := filepath.Join(modelsRoot, modelPath)

	// Check if directory exists and has files
	info, err := os.Stat(fullPath)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"exists": false,
			"model":  model,
			"format": format,
		})
		return
	}

	if !info.IsDir() {
		c.JSON(http.StatusOK, gin.H{
			"exists": false,
			"model":  model,
			"format": format,
		})
		return
	}

	// Check if directory has any model files
	hasFiles := false
	var totalSize int64

	_ = filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // keep walking on per-entry errors (permission denied, stale symlink)
		}
		if !info.IsDir() && modelsync.IsModelFile(info.Name()) {
			hasFiles = true
			totalSize += info.Size()
		}
		return nil
	})

	c.JSON(http.StatusOK, gin.H{
		"exists":     hasFiles,
		"model":      model,
		"format":     format,
		"total_size": totalSize,
	})
}

// SyncPullRequest represents a request for a worker to sync from master
type SyncPullRequest struct {
	Download   *metadata.DownloadRequest `json:"download"`
	Provider   string                    `json:"provider,omitempty"`
	Features   []string                  `json:"features,omitempty"`
	Model      string                    `json:"model" binding:"required"`
	Format     string                    `json:"format"`
	SourceNode string                    `json:"source_node" binding:"required"` // Node to deploy from
	SourceURL  string                    `json:"source_url" binding:"required"`  // URL of source node
	JobID      string                    `json:"job_id"`                         // Parent job ID
}

// HandleSyncPull handles a sync deploy request from the coordinator
// POST /zzrouter/internal/sync/deploy
// This endpoint is called by the coordinator to tell a worker to sync a model
// Security: Requires cluster key, validates source URL, uses atomic writes
func (e *SyncExecutor) HandleSyncPull(c *gin.Context) {
	var req SyncPullRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if req.Format == "" {
		req.Format = "gguf"
	}
	if req.Download != nil && req.Download.Repo != req.Model {
		BadRequest(c, "download plan does not match model")
		return
	}

	// Validate format against whitelist
	if !allowedSyncFormats[req.Format] {
		BadRequest(c, fmt.Sprintf("Format %q is not supported. Allowed formats: gguf, safetensors, mlx, bin, pt", req.Format))
		return
	}

	if err := e.validateSyncSourceURL(req.SourceURL); err != nil {
		BadRequest(c, "rejected sync source: "+err.Error())
		return
	}

	// Dedupe retry storms: a second deploy for the same (model, format)
	// while the first is still running returns 409 with the existing job.

	slog.Info("[SyncExecutor] SyncPull: source=", "model", req.Model, "format", req.Format, "source", req.SourceNode, "source_url", req.SourceURL, "job", req.JobID)

	mtls := e.mtlsClient()
	if mtls == nil {
		ServiceUnavailable(c, "mTLS dispatch client unavailable: node must be a paired worker")
		return
	}

	// Rewrite the coordinator-supplied sourceURL onto the cluster port.
	// The coordinator advertises peers by their public URL; sync runs
	// over the mTLS cluster port. DeriveClusterURL swaps scheme + port.
	clusterSourceURL, err := mesh.DeriveClusterURL(req.SourceURL, e.clusterPort)
	if err != nil {
		BadRequest(c, fmt.Sprintf("derive cluster URL from %s: %v", req.SourceURL, err))
		return
	}

	syncClient := modelsync.NewClient(modelsync.ClientConfig{
		HTTPClient: mtls,
	})
	if syncClient == nil {
		ServiceUnavailable(c, "sync client unavailable")
		return
	}

	// Open a local KindSync jobs.Handle so subscribers can follow
	// progress via /zzrouter/v1/jobs/:id/stream?node=<worker>. The
	// handle ID returned to the caller is the subscribable one — any
	// upstream parent (req.JobID) is carried in Meta for tracing but
	// isn't the live producer here.
	var handle jobs.Handle
	if e.jobs != nil {
		meta := jobs.Meta{
			"model":  req.Model,
			"format": req.Format,
			"source": req.SourceNode,
		}
		if req.JobID != "" {
			meta["parent_job_id"] = req.JobID
		}
		// StartDetached so the job outlives the HTTP accept path; the
		// background goroutine below runs on the server's shutdown ctx
		// and the handle's lifetime matches.
		h, err := e.jobs.StartDetached(jobs.KindSync, "", meta)
		if err == nil {
			handle = h
		}
	}
	localJobID := ""
	if handle != nil {
		localJobID = handle.ID()
	}
	inflightKey := req.Model + "|" + req.Format + "|" + deployPlanKey(req.Provider, req.Features, req.Download)
	if existing, claimed := e.claimInflightDeploy(inflightKey, localJobID); !claimed {
		if handle != nil {
			handle.Fail(fmt.Errorf("sync already in progress: %s", existing))
		}
		c.JSON(http.StatusConflict, gin.H{"status": "conflict", "model": req.Model, "format": req.Format, "existing_job_id": existing})
		return
	}

	// Start sync in background using server shutdown context for graceful cleanup
	go e.executeSyncPull(req, handle, inflightKey, syncClient, clusterSourceURL)

	// Return accepted status immediately. Caller should subscribe to
	// local_job_id for live progress; req.JobID is echoed for trace
	// correlation with any upstream parent job.
	c.JSON(http.StatusAccepted, gin.H{
		"status":       "accepted",
		"model":        req.Model,
		"format":       req.Format,
		"source":       req.SourceNode,
		"job_id":       req.JobID,
		"local_job_id": localJobID,
		"message":      "Sync started in background",
	})
}

func (e *SyncExecutor) executeSyncPull(req SyncPullRequest, handle jobs.Handle, inflightKey string, syncClient *modelsync.Client, clusterSourceURL string) {
	defer e.releaseInflightDeploy(inflightKey)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PANIC recovered in sync executor", "panic", r, "stack", string(debug.Stack()))
			if handle != nil {
				handle.Fail(fmt.Errorf("sync panic: %v", r))
			}
		}
	}()

	// Use shutdown context so sync stops on server shutdown. When a
	// jobs.Handle is present, its context carries the DELETE /jobs
	// cancel signal — intersect with shutdown so either can tear
	// down the sync cleanly.
	syncCtx := e.shutdownCtx()
	if handle != nil {
		hctx := handle.Context()
		merged, cancel := context.WithCancel(syncCtx)
		go func() {
			select {
			case <-hctx.Done():
				cancel()
			case <-merged.Done():
			}
		}()
		defer cancel()
		syncCtx = merged
	}

	progressCallback := func(progress modelsync.Progress) {
		slog.Info("[SyncExecutor] Progress",
			"file", progress.File,
			"progress", progress.Progress,
			"files_completed", progress.FilesCompleted,
			"files_total", progress.FilesTotal)
		if handle == nil {
			return
		}
		pct := int(progress.Progress)
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		handle.Progress(pct, progress.File, jobs.Bytes{
			Done:  progress.BytesDownloaded,
			Total: progress.BytesTotal,
		})
		handle.Meta(jobs.Meta{
			"files_completed": progress.FilesCompleted,
			"files_total":     progress.FilesTotal,
		})
	}

	request := metadata.DownloadRequest{Repo: req.Model}
	if req.Download != nil {
		request = *req.Download
	}
	result, err := syncClient.SyncModel(syncCtx, clusterSourceURL, request, req.Format, progressCallback)
	if err != nil {
		slog.Error("Sync failed", "model", req.Model, "job", req.JobID, "source", req.SourceNode, "error", err)
		if handle != nil {
			handle.Fail(err)
		}
		return
	}
	if result.Success && req.Provider != "" && e.ensureFeatures != nil {
		root, ferr := modelregistry.GetModelsRootDir()
		if ferr == nil {
			ferr = e.ensureFeatures(syncCtx, req.Provider, filepath.Join(root, request.Repo), req.Features, request.Optional, func(message string) {
				if handle != nil {
					handle.Progress(99, message, jobs.Bytes{})
				}
			})
		}
		if ferr != nil {
			if handle != nil {
				handle.Fail(ferr)
			}
			return
		}
	}

	slog.Info("[SyncExecutor] Sync complete",
		"model", req.Model, "job", req.JobID,
		"files_downloaded", result.FilesDownloaded,
		"files_skipped", result.FilesSkipped,
		"files_failed", result.FilesFailed)

	if result.Success && e.refreshIndex != nil {
		_ = e.refreshIndex()
	}

	if handle != nil {
		handle.Meta(jobs.Meta{
			"files_downloaded": result.FilesDownloaded,
			"files_skipped":    result.FilesSkipped,
			"files_failed":     result.FilesFailed,
		})
		if result.Success {
			handle.Done()
		} else {
			handle.Fail(fmt.Errorf("sync incomplete: %d files failed", result.FilesFailed))
		}
	}
}

// validateSyncSourceURL accepts a /sync/deploy source URL only when it
// resolves to an IP backed by a known cluster member. Without this
// check a leaked cluster key gives arbitrary outbound HTTP to any
// host — the classic SSRF pivot from a shared secret.
func (e *SyncExecutor) validateSyncSourceURL(sourceURL string) error {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (http/https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}

	// Resolve host to IPs. This is a DNS lookup on the sync coordinator's
	// resolver; hostnames that resolve only on the attacker's network are
	// still rejected because they don't match any cluster member IP below.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("host %q did not resolve: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q resolved to zero IPs", host)
	}

	// Reject IP properties that have no legitimate sync source meaning.
	for _, ip := range ips {
		if ip.IsUnspecified() {
			return fmt.Errorf("host %q resolves to unspecified address", host)
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("host %q resolves to link-local address %s", host, ip)
		}
		if ip.IsMulticast() {
			return fmt.Errorf("host %q resolves to multicast address %s", host, ip)
		}
	}

	// Build the set of legitimate cluster-member hostnames/IPs from the
	// registry. Two degraded modes are tolerated:
	//
	//  1. Standalone (cluster == nil): the node has no cluster at all.
	//     Only loopback source URLs are allowed — this covers the tests
	//     that spin up httptest servers on 127.0.0.1. Any non-loopback
	//     source is a config error on a non-clustered node.
	//  2. Clustered but no endpoints yet (pre-join window): same
	//     loopback-only fallback applies.
	//
	// In normal clustered operation the registry has at least one
	// endpoint and the loopback fallback is not reached.
	allowedHosts := map[string]struct{}{}
	if eps := e.getEndpoints(); len(eps) > 0 {
		for _, ep := range eps {
			epURL, err := url.Parse(ep.URL)
			if err != nil || epURL.Hostname() == "" {
				continue
			}
			allowedHosts[strings.ToLower(epURL.Hostname())] = struct{}{}
		}
	}
	if len(allowedHosts) == 0 {
		// Loopback-only fallback: all resolved IPs must be loopback.
		for _, ip := range ips {
			if !ip.IsLoopback() {
				return fmt.Errorf("no cluster endpoints registered; only loopback sync sources are allowed (got %s)", ip)
			}
		}
		return nil
	}

	// Fast path: the URL host is already registered verbatim.
	if _, ok := allowedHosts[strings.ToLower(host)]; ok {
		return nil
	}

	// Slow path: resolve each allowed host and compare IP sets. Covers
	// the case where the registry stores by hostname but the request
	// uses the IP literal, or vice versa.
	resolvedAllowed := map[string]struct{}{}
	for allowed := range allowedHosts {
		for _, ip := range resolveAllIPs(allowed) {
			resolvedAllowed[ip.String()] = struct{}{}
		}
	}
	for _, ip := range ips {
		if _, ok := resolvedAllowed[ip.String()]; ok {
			return nil
		}
	}

	return fmt.Errorf("host %q (resolved to %v) is not a registered cluster member", host, ips)
}

// resolveAllIPs is a lookup helper that returns the empty slice on
// resolution failure so the caller can keep walking the allow-list.
func resolveAllIPs(host string) []net.IP {
	// If the allow-list entry is already an IP literal, skip DNS.
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil
	}
	return ips
}
