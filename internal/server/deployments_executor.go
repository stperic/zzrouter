package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/autoroute"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ============================================================================
// Deployments Executor - Internal API for Direct Local Execution
// ============================================================================
//
// Handles direct interaction with download systems for /zzrouter/v1/internal/
// deployments endpoints (cluster-internal). Responsibilities:
//
// - Execute downloads locally (Ollama, HuggingFace)
// - Track download progress
// - No routing, no caching, no orchestration
// - Return raw local data only
//
// Response shapes are typed (internalDeploymentResult, StopDownloadResponse, etc.)
// so DeploymentsService can unmarshal them via routeAndParse[T]. Error paths
// use the Problem Details helpers so the public controller can propagate
// status codes faithfully via RespondToError.

// DownloadSummary is the per-download entry produced by the internal list
// endpoint. The public surface exposes deployments instead (Deployment.Nodes
// carries equivalent byte/progress data); this type is consumed only by
// cluster-internal callers and the executor itself.
type DownloadSummary struct {
	Key             string  `json:"key"`
	Model           string  `json:"model"`
	Node            string  `json:"node"`
	Status          string  `json:"status"`
	CurrentFile     string  `json:"current_file,omitempty"`
	FilesDone       int     `json:"files_done,omitempty"`
	FilesTotal      int     `json:"files_total,omitempty"`
	Progress        float64 `json:"progress"`
	BytesDownloaded int64   `json:"bytes_downloaded"`
	BytesTotal      int64   `json:"bytes_total"`
	Speed           int64   `json:"speed"`
	StartedAt       string  `json:"started_at"`
	ETA             string  `json:"eta"`
	ETASeconds      int     `json:"eta_seconds"`
	Error           string  `json:"error,omitempty"`
	Message         string  `json:"message,omitempty"`
}

// listDeploymentsEnvelope is the wire shape for GET /zzrouter/v1/internal/deployments.
type listDeploymentsEnvelope struct {
	Data []DownloadSummary `json:"data"`
}

// DeploymentsExecutor handles direct local download operations for the
// cluster-internal deployments surface.
type DeploymentsExecutor struct {
	getNodename            func() string
	isCloudProviderFn      func(string) bool
	isLocalNodeCompatible  func(string, string, string) bool
	buildIncompatibleError func(string, string, string) string
	downloads              *modelregistry.DownloadTracker
	registry               *modelregistry.Registry
	autoRoute              *autoroute.Manager
	ollamaDaemon           func() (*backend.Resolved, error)
	appsConfig             func() *pkgConfig.AppsConfig
	configStore            *pkgConfig.AppsConfigStore
	refreshCacheSync       func(context.Context) error
	jobs                   *jobs.Registry
	ensureFeatures         func(context.Context, string, string, []string, []string, func(string)) error

	// Dedup: downloadKey → live jobs.Handle ID. A retry POST for an
	// already-running download must return the same JobID so the second
	// client can subscribe to the same SSE stream. Cleared when the
	// handle reaches a terminal phase.
	jobKeysMu sync.Mutex
	jobKeys   map[string]string
}

// NewDeploymentsExecutor wires a DeploymentsExecutor.
func NewDeploymentsExecutor(
	getNodename func() string,
	isCloudProvider func(string) bool,
	isLocalNodeCompatible func(string, string, string) bool,
	buildIncompatibleError func(string, string, string) string,
	downloads *modelregistry.DownloadTracker,
	registry *modelregistry.Registry,
	autoRoute *autoroute.Manager,
	ollamaDaemon func() (*backend.Resolved, error),
	appsConfig func() *pkgConfig.AppsConfig,
	configStore *pkgConfig.AppsConfigStore,
	refreshCacheSync func(context.Context) error,
	jobsReg *jobs.Registry,
) *DeploymentsExecutor {
	return &DeploymentsExecutor{
		getNodename:            getNodename,
		isCloudProviderFn:      isCloudProvider,
		isLocalNodeCompatible:  isLocalNodeCompatible,
		buildIncompatibleError: buildIncompatibleError,
		downloads:              downloads,
		registry:               registry,
		autoRoute:              autoRoute,
		ollamaDaemon:           ollamaDaemon,
		appsConfig:             appsConfig,
		configStore:            configStore,
		refreshCacheSync:       refreshCacheSync,
		jobs:                   jobsReg,
		jobKeys:                make(map[string]string),
	}
}

// newDeploymentResult constructs the typed response the service layer
// unmarshals from internal deploy calls. jobID is optional — empty
// when the call is a dedupe early-return without a new producer.
func (e *DeploymentsExecutor) newDeploymentResult(key, model, registry, status, message string) internalDeploymentResult {
	return internalDeploymentResult{
		Key:      key,
		Model:    model,
		Registry: registry,
		Node:     e.getNodename(),
		Status:   status,
		Message:  message,
	}
}

// openDownloadJob opens a jobs.Handle tracking this download and
// indexes it by downloadKey so a dedupe retry returns the same JobID.
// Returns nil if the jobs registry is unavailable or Start fails.
func (e *DeploymentsExecutor) openDownloadJob(c *gin.Context, downloadKey, modelName, repoName string) jobs.Handle {
	if e.jobs == nil {
		return nil
	}
	h, err := e.jobs.Start(context.Background(), jobs.KindDownload,
		PrincipalFromContext(c),
		jobs.Meta{"model": modelName, "registry": repoName, "key": downloadKey})
	if err != nil {
		slog.Warn("[DeploymentsExecutor] jobs.Start failed", "error", err, "model", modelName)
		return nil
	}
	e.jobKeysMu.Lock()
	e.jobKeys[downloadKey] = h.ID()
	e.jobKeysMu.Unlock()
	return h
}

// activeJobIDForKey returns the live jobs.Handle ID associated with a
// downloadKey, or "" if none. Used by dedupe early-return paths so a
// retry POST can hand the client a subscribe target.
func (e *DeploymentsExecutor) activeJobIDForKey(downloadKey string) string {
	e.jobKeysMu.Lock()
	defer e.jobKeysMu.Unlock()
	return e.jobKeys[downloadKey]
}

// forgetJobKey releases the downloadKey → jobID association once the
// producer has signalled terminal state. Called from publishJobEvent on
// completed/failed/cancelled.
func (e *DeploymentsExecutor) forgetJobKey(downloadKey string) {
	e.jobKeysMu.Lock()
	delete(e.jobKeys, downloadKey)
	e.jobKeysMu.Unlock()
}

// publishJobEvent translates a DownloadTracker status update into the
// corresponding pkg/jobs emission. Idempotent against a nil handle and
// against post-terminal calls (pkg/jobs drops late calls silently).
// `progress` is the 0–100 percent used by the tracker; bytes carries
// the {done,total} pair the SSE envelope surfaces. On terminal, the
// downloadKey → jobID index is cleared.
//
// On failed/cancelled, the modelregistry-supplied `currentFile` carries
// the upstream error message (by convention — see registry_pull.go's
// statusUpdater calls). Threading it into h.Fail preserves operator-
// debug parity with the pre-jobs flow.
func (e *DeploymentsExecutor) publishJobEvent(h jobs.Handle, downloadKey, status, currentFile string, progress float64, downloaded, totalSize int64) {
	if h == nil {
		return
	}
	pct := int(progress)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	bytes := jobs.Bytes{Done: downloaded, Total: totalSize}
	switch status {
	case string(constants.StatusCompleted):
		h.Done()
		e.forgetJobKey(downloadKey)
	case string(constants.StatusFailed):
		reason := "download failed"
		if currentFile != "" {
			reason = "download failed: " + currentFile
		}
		h.Fail(errors.New(reason))
		e.forgetJobKey(downloadKey)
	case string(constants.StatusCancelled):
		reason := "download cancelled"
		if currentFile != "" {
			reason = "download cancelled: " + currentFile
		}
		h.Fail(errors.New(reason))
		e.forgetJobKey(downloadKey)
	default:
		h.Progress(pct, currentFile, bytes)
	}
}

// HandleInternalDeploy handles POST /zzrouter/v1/internal/deployments.
func (e *DeploymentsExecutor) HandleInternalDeploy(c *gin.Context) {
	var req struct {
		Provider string                    `json:"provider,omitempty"`
		Features []string                  `json:"features,omitempty"`
		Download *metadata.DownloadRequest `json:"download"`
		Model    string                    `json:"model" binding:"required"`
		Node     string                    `json:"node,omitempty"`
		Registry string                    `json:"registry,omitempty"`
		File     string                    `json:"file,omitempty"`
		Force    bool                      `json:"force,omitempty"`
	}

	if !BindJSON(c, &req) {
		return
	}

	// Parse with cloud-aware hints. For a cloud Registry, Parse short-circuits
	// and returns the model name verbatim — preserving '@', '#', ':', '/' that
	// carry different semantics in cloud model IDs (e.g., "@cf/meta/llama-3",
	// "openai/gpt-4o-mini:2024-07-18").
	id, err := modelregistry.Parse(req.Model, modelregistry.ParseOpts{
		Config:   e.appsConfig(),
		Registry: req.Registry,
	})
	if err != nil {
		BadRequest(c, fmt.Sprintf("invalid model identifier: %v", err))
		return
	}

	modelName := id.GetFullModelName()
	if modelName == "" {
		BadRequest(c, "model name is required")
		return
	}

	repoName := req.Registry
	if repoName == "" && id.Repository != "" {
		repoName = id.Repository
	}
	if repoName == "" {
		// Auto-detect: HuggingFace models have slashes, Ollama doesn't
		if strings.Contains(modelName, "/") {
			repoName = constants.RepoHuggingFace
		} else {
			repoName = constants.RepoOllama
		}
	}

	// Compatibility gate: reject downloads to nodes with no compatible provider.
	// Skip check for: forced pulls, cloud variants, cloud providers.
	skipCompatCheck := req.Force ||
		isOllamaCloudVariant(modelName, req.File) ||
		e.isCloudProvider(repoName) ||
		e.hasProviderCredentials(repoName)

	hostname := e.getNodename()

	if !skipCompatCheck && !e.isLocalNodeCompatible(modelName, repoName, "") {
		errMsg := e.buildIncompatibleError(hostname, modelName, repoName)
		slog.Warn("[DeploymentsExecutor] Blocked deploy: no compatible provider on node",
			"model", modelName, "registry", repoName, "node", hostname)
		BadRequest(c, errMsg)
		return
	}

	// Create download key for tracking
	downloadKey := fmt.Sprintf("%s/%s/%s", hostname, repoName, modelName)
	if req.Download != nil || req.Provider != "" || len(req.Features) > 0 {
		downloadKey += "/" + deployPlanKey(req.Provider, req.Features, req.Download)
	}

	// Route to appropriate handler based on repository
	switch strings.ToLower(repoName) {
	case constants.RepoOllama:
		// Cloud variant (e.g., "minimax-m2.7" with file="cloud") → register, don't download
		if isOllamaCloudVariant(modelName, req.File) {
			cloudModelName := modelName
			if req.File != "" && !strings.Contains(modelName, ":") {
				cloudModelName = modelName + ":" + req.File
			}
			cloudKey := fmt.Sprintf("%s/%s/%s", hostname, repoName, cloudModelName)
			e.registerCloudModel(c, repoName, cloudModelName, cloudKey)
		} else {
			e.downloadOllama(c, modelName, downloadKey)
		}
	case constants.RepoHuggingFace, constants.RepoHFAlias:
		d := metadata.DownloadRequest{Repo: modelName, Weights: req.File, Force: req.Force}
		if req.Download != nil {
			d = *req.Download
			if d.Repo != modelName || d.Force != req.Force {
				BadRequest(c, "download plan does not match deployment")
				return
			}
		}
		e.downloadHuggingFace(c, d, downloadKey, req.Provider, req.Features)
	default:
		// Cloud provider (openrouter, groq, etc.) → check credentials and register
		if e.isCloudProvider(repoName) || e.hasProviderCredentials(repoName) {
			e.registerCloudModel(c, repoName, modelName, downloadKey)
		} else {
			BadRequest(c, fmt.Sprintf("unsupported repository: %s (supported: ollama, huggingface, or cloud providers)", repoName))
		}
	}
}

// downloadOllama handles Ollama model downloads.
func (e *DeploymentsExecutor) downloadOllama(c *gin.Context, modelName, downloadKey string) {
	_, err := e.ollamaDaemon()
	if err != nil {
		NotFound(c, "Ollama not configured")
		return
	}

	// Deduplicate in-flight downloads against the tracker.
	if existing := e.downloads.GetDownloadStatus(downloadKey); existing != nil {
		if !modelregistry.IsTerminalStatus(existing.Status) {
			result := e.newDeploymentResult(
				downloadKey, modelName, constants.RepoOllama,
				existing.Status,
				fmt.Sprintf("Download already in progress for ollama/%s", modelName),
			)
			result.JobID = e.activeJobIDForKey(downloadKey)
			c.JSON(http.StatusOK, result)
			return
		}
	}

	// StartDownload cancels any previous download for the same key and returns
	// a generation number passed to UpdateDownloadFullGen so that a stop+redeploy
	// cycle silently rejects late writes from the old goroutine.
	gen := e.downloads.StartDownload(downloadKey, modelName)
	job := e.openDownloadJob(c, downloadKey, modelName, constants.RepoOllama)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("PANIC recovered in Ollama download", "panic", r, "stack", string(debug.Stack()))
				e.downloads.UpdateDownloadFullGen(downloadKey, gen, string(constants.StatusFailed), "", 0, 0, 0, 0, 0, 0)
				if job != nil {
					job.Fail(fmt.Errorf("panic: %v", r))
				}
			}
		}()

		// 48-hour safety timeout prevents orphaned contexts if StopDownload is
		// never called. Normal downloads complete much faster or are cancelled.
		ctx, cancel := e.downloadContext(job, downloadKey)

		statusUpdater := func(status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64) {
			if modelregistry.IsTerminalStatus(status) {
				defer cancel()
			}
			select {
			case <-ctx.Done():
				e.downloads.UpdateDownloadFullGen(downloadKey, gen, string(constants.StatusCancelled), currentFile, filesDownloaded, filesTotal, progress, downloaded, totalSize, 0)
				e.publishJobEvent(job, downloadKey, string(constants.StatusCancelled), currentFile, progress, downloaded, totalSize)
				return
			default:
				e.downloads.UpdateDownloadFullGen(downloadKey, gen, status, currentFile, filesDownloaded, filesTotal, progress, downloaded, totalSize, speed)
				e.publishJobEvent(job, downloadKey, status, currentFile, progress, downloaded, totalSize)
			}

			if status == string(constants.StatusCompleted) && e.autoRoute != nil {
				e.autoRoute.SyncAfterPull()
			}
		}

		if e.registry != nil {
			e.registry.PullOllamaModelAsync(ctx, modelName, statusUpdater)
		}
	}()

	result := e.newDeploymentResult(
		downloadKey, modelName, constants.RepoOllama,
		string(constants.StatusDownloading),
		fmt.Sprintf("Download initiated for ollama/%s", modelName),
	)
	if job != nil {
		result.JobID = job.ID()
	}
	c.JSON(http.StatusAccepted, result)
}

// downloadHuggingFace handles HuggingFace model downloads.
func (e *DeploymentsExecutor) downloadHuggingFace(c *gin.Context, request metadata.DownloadRequest, downloadKey, provider string, features []string) {
	modelName := request.Repo
	if e.registry == nil {
		ServiceUnavailable(c, "model registry not initialized")
		return
	}

	if !strings.Contains(modelName, "/") {
		BadRequest(c, fmt.Sprintf("HuggingFace model name must include the organization prefix (e.g., 'org/%s')", modelName))
		return
	}

	if existing := e.downloads.GetDownloadStatus(downloadKey); existing != nil {
		if !modelregistry.IsTerminalStatus(existing.Status) {
			result := e.newDeploymentResult(
				downloadKey, modelName, constants.RepoHuggingFace,
				existing.Status,
				fmt.Sprintf("Download already in progress for huggingface/%s", modelName),
			)
			result.JobID = e.activeJobIDForKey(downloadKey)
			c.JSON(http.StatusOK, result)
			return
		}
	}

	gen := e.downloads.StartDownload(downloadKey, modelName)
	job := e.openDownloadJob(c, downloadKey, modelName, constants.RepoHuggingFace)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("PANIC recovered in HuggingFace download", "panic", r, "stack", string(debug.Stack()))
				e.downloads.UpdateDownloadFullGen(downloadKey, gen, string(constants.StatusFailed), "", 0, 0, 0, 0, 0, 0)
				if job != nil {
					job.Fail(fmt.Errorf("panic: %v", r))
				}
			}
		}()

		ctx, cancel := e.downloadContext(job, downloadKey)

		statusUpdater := func(status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64) {
			if modelregistry.IsTerminalStatus(status) {
				defer cancel()
			}
			if status == string(constants.StatusCompleted) && provider != "" && e.ensureFeatures != nil {
				root, err := modelregistry.GetModelsRootDir()
				if err == nil {
					err = e.ensureFeatures(ctx, provider, filepath.Join(root, request.Repo), features, request.Optional, func(message string) {
						if job != nil {
							job.Progress(99, message, jobs.Bytes{Done: downloaded, Total: totalSize})
						}
					})
				}
				if err != nil {
					status, currentFile = string(constants.StatusFailed), err.Error()
				}
			}
			select {
			case <-ctx.Done():
				e.downloads.UpdateDownloadFullGen(downloadKey, gen, string(constants.StatusCancelled), currentFile, filesDownloaded, filesTotal, progress, downloaded, totalSize, 0)
				e.publishJobEvent(job, downloadKey, string(constants.StatusCancelled), currentFile, progress, downloaded, totalSize)
				return
			default:
				e.downloads.UpdateDownloadFullGen(downloadKey, gen, status, currentFile, filesDownloaded, filesTotal, progress, downloaded, totalSize, speed)
				e.publishJobEvent(job, downloadKey, status, currentFile, progress, downloaded, totalSize)
			}

			if status == string(constants.StatusCompleted) && e.autoRoute != nil {
				e.autoRoute.SyncAfterPull()
			}
		}

		e.registry.DownloadHuggingFaceModelAsync(ctx, request, statusUpdater)
	}()

	result := e.newDeploymentResult(
		downloadKey, modelName, constants.RepoHuggingFace,
		string(constants.StatusDownloading),
		fmt.Sprintf("Download initiated for huggingface/%s", modelName),
	)
	if job != nil {
		result.JobID = job.ID()
	}
	c.JSON(http.StatusAccepted, result)
}

// HandleInternalListDeployments handles GET /zzrouter/v1/internal/deployments.
func (e *DeploymentsExecutor) HandleInternalListDeployments(c *gin.Context) {
	if e.downloads == nil {
		c.JSON(http.StatusOK, listDeploymentsEnvelope{Data: []DownloadSummary{}})
		return
	}

	downloadEntries := e.downloads.GetStatus()

	hostname := e.getNodename()
	result := make([]DownloadSummary, 0, len(downloadEntries))

	for _, entry := range downloadEntries {
		s := entry.Status
		etaSeconds := 0
		if remaining := time.Until(s.ETA); remaining > 0 {
			etaSeconds = int(remaining.Seconds())
		}

		result = append(result, DownloadSummary{
			Key:             entry.Key,
			Model:           s.ModelName,
			Node:            hostname,
			Status:          s.Status,
			CurrentFile:     s.CurrentFile,
			FilesDone:       s.FilesDownloaded,
			FilesTotal:      s.FilesTotal,
			Progress:        s.Progress,
			BytesDownloaded: s.Downloaded,
			BytesTotal:      s.TotalSize,
			Speed:           s.Speed,
			StartedAt:       s.StartedAt.Format(time.RFC3339),
			ETA:             s.ETA.Format(time.RFC3339),
			ETASeconds:      etaSeconds,
			Error:           s.Error,
			Message:         s.Message,
		})
	}

	slog.Debug("[DeploymentsExecutor] list: returning downloads", "count", len(result), "hostname", hostname)
	c.JSON(http.StatusOK, listDeploymentsEnvelope{Data: result})
}

// HandleInternalStopDownload handles DELETE /zzrouter/v1/internal/deployments/stop?key=
// Supports both active downloads (cancels them) and terminal-state entries
// (removes them). A cancelled download removes its own partial file.
func (e *DeploymentsExecutor) HandleInternalStopDownload(c *gin.Context) {
	downloadID := c.Query("key")
	if downloadID == "" {
		BadRequest(c, "download ID is required")
		return
	}

	if e.downloads == nil {
		ServiceUnavailable(c, "download tracker not initialized")
		return
	}

	// Try to stop active download first (has cancel func)
	if e.downloads.StopDownload(downloadID) {
		e.downloads.RemoveDownload(downloadID)

		slog.Info("[DeploymentsExecutor] Stopped and removed active download", "download_id", downloadID)
		c.JSON(http.StatusOK, StopDownloadResponse{
			Key:     downloadID,
			Status:  "removed",
			Message: "Download stopped and entry removed",
		})
		return
	}

	// Not active — check if it's a terminal-state entry.
	status := e.downloads.GetDownloadStatus(downloadID)
	if status == nil {
		NotFound(c, fmt.Sprintf("download not found: %s", downloadID))
		return
	}

	if modelregistry.IsTerminalStatus(status.Status) {
		e.downloads.RemoveDownload(downloadID)

		slog.Info("[DeploymentsExecutor] Removed terminal download entry", "download_id", downloadID, "was_status", status.Status)
		c.JSON(http.StatusOK, StopDownloadResponse{
			Key:     downloadID,
			Status:  "removed",
			Message: fmt.Sprintf("Download entry removed (was %s)", status.Status),
		})
		return
	}

	Conflict(c, fmt.Sprintf("download %s is in state '%s' but cannot be cancelled", downloadID, status.Status))
}

func (e *DeploymentsExecutor) isCloudProvider(repoName string) bool {
	return e.isCloudProviderFn(repoName)
}

// hasProviderCredentials reports whether the provider has API credentials configured.
func (e *DeploymentsExecutor) hasProviderCredentials(repoName string) bool {
	cfg := e.appsConfig()
	if cfg == nil || repoName == "" {
		return false
	}
	appCfg, exists := cfg.LookupApp(repoName)
	if !exists {
		return false
	}
	return appCfg.Runtime != nil && appCfg.Runtime.API != nil && appCfg.Runtime.API.HasCredentials()
}

// isOllamaCloudVariant returns true if the Ollama model deploy targets a cloud
// variant. Cloud variants are identified by the "cloud" tag suffix (e.g.,
// file="cloud" or model ends with ":cloud").
func isOllamaCloudVariant(modelName, file string) bool {
	if strings.EqualFold(file, "cloud") {
		return true
	}
	if idx := strings.LastIndex(modelName, ":"); idx >= 0 {
		tag := modelName[idx+1:]
		if strings.EqualFold(tag, "cloud") || strings.HasSuffix(strings.ToLower(tag), "-cloud") {
			return true
		}
	}
	return false
}

// registerCloudModel registers a model with a provider's capabilities.models
// in provider config, updates the in-memory config, and refreshes the model
// cache. Cloud models don't need downloading, just registration. Works for
// any provider with API credentials (cloud providers, Ollama with API key).
func (e *DeploymentsExecutor) registerCloudModel(c *gin.Context, providerName, modelName, downloadKey string) {
	if err := e.configStore.AddCloudModel(providerName, modelName); err != nil {
		BadRequest(c, err.Error())
		return
	}

	if err := e.refreshCacheSync(c.Request.Context()); err != nil {
		slog.Warn("Cache refresh after cloud model registration failed", "error", err)
	}

	slog.Info("Cloud model registered", "provider", providerName, "model", modelName)

	e.autoRoute.SyncAfterPull()

	c.JSON(http.StatusOK, e.newDeploymentResult(
		downloadKey, modelName, providerName,
		string(constants.StatusCompleted),
		fmt.Sprintf("Model '%s' registered with cloud provider '%s'", modelName, providerName),
	))
}

// HandleInternalStopAllDownloads handles DELETE /zzrouter/v1/internal/deployments/all.
func (e *DeploymentsExecutor) HandleInternalStopAllDownloads(c *gin.Context) {
	if e.downloads == nil {
		c.JSON(http.StatusOK, StopAllDownloadsResponse{
			Stopped: 0,
			Message: "No downloads to stop",
		})
		return
	}

	cancelledKeys := e.downloads.StopAll()

	slog.Info("[DeploymentsExecutor] Stopped downloads", "count", len(cancelledKeys))
	c.JSON(http.StatusOK, StopAllDownloadsResponse{
		Stopped: len(cancelledKeys),
		Message: fmt.Sprintf("Stopped %d download(s)", len(cancelledKeys)),
	})
}

// downloadContext ties installer and transfer cancellation to the same job and
// tracker. Without a jobs registry, the tracker still owns cancellation.
func (e *DeploymentsExecutor) downloadContext(job jobs.Handle, key string) (context.Context, context.CancelFunc) {
	parent := context.Background()
	if job != nil {
		parent = job.Context()
	}
	ctx, cancel := context.WithTimeout(parent, 48*time.Hour)
	e.downloads.RegisterCancelFunc(key, cancel)
	return ctx, cancel
}
