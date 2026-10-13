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
	ollamaDaemon           func() (*backend.Resolved, error)
	appsConfig             func() *pkgConfig.AppsConfig
	configStore            *pkgConfig.AppsConfigStore
	publishCatalog         func() error
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
	ollamaDaemon func() (*backend.Resolved, error),
	appsConfig func() *pkgConfig.AppsConfig,
	configStore *pkgConfig.AppsConfigStore,
	publishCatalog func() error,
	jobsReg *jobs.Registry,
) *DeploymentsExecutor {
	return &DeploymentsExecutor{
		getNodename:            getNodename,
		isCloudProviderFn:      isCloudProvider,
		isLocalNodeCompatible:  isLocalNodeCompatible,
		buildIncompatibleError: buildIncompatibleError,
		downloads:              downloads,
		registry:               registry,
		ollamaDaemon:           ollamaDaemon,
		appsConfig:             appsConfig,
		configStore:            configStore,
		publishCatalog:         publishCatalog,
		jobs:                   jobsReg,
		jobKeys:                make(map[string]string),
	}
}

// newDeploymentResult constructs the typed response the service layer
// unmarshals from internal deploy calls. Only synchronous terminal results
// omit JobID; asynchronous dedupe returns the existing attempt.
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

type downloadAttempt struct {
	job        jobs.Handle
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
}

// beginDownload atomically deduplicates, admits the observable job, and binds
// cancellation before returning acceptance. Refusal starts no physical work.
func (e *DeploymentsExecutor) beginDownload(c *gin.Context, key, model, repo string) *downloadAttempt {
	e.jobKeysMu.Lock()
	defer e.jobKeysMu.Unlock()
	if e.jobs == nil || e.downloads == nil {
		ServiceUnavailable(c, "download job registry unavailable")
		return nil
	}
	if existing := e.downloads.GetDownloadStatus(key); existing != nil && !modelregistry.IsTerminalStatus(existing.Status) {
		id := e.jobKeys[key]
		ev, err := e.jobs.Get(id)
		if err != nil || ev.Phase.IsTerminal() {
			ServiceUnavailable(c, "existing download has no observable live job; stop it before retrying")
			return nil
		}
		result := e.newDeploymentResult(key, model, repo, existing.Status, "Download already in progress")
		result.JobID = id
		c.JSON(http.StatusOK, result)
		return nil
	}
	h, err := e.jobs.StartDetached(jobs.KindDownload, PrincipalFromContext(c), jobs.Meta{"model": model, "registry": repo, "key": key})
	if err != nil {
		ServiceUnavailable(c, "start download job: "+err.Error())
		return nil
	}
	ctx, cancel := context.WithTimeout(h.Context(), 48*time.Hour)
	gen := e.downloads.StartDownload(key, model, cancel)
	e.jobKeys[key] = h.ID()
	return &downloadAttempt{job: h, generation: gen, ctx: ctx, cancel: cancel}
}

func (e *DeploymentsExecutor) forgetJobKey(key, jobID string) {
	e.jobKeysMu.Lock()
	if e.jobKeys[key] == jobID {
		delete(e.jobKeys, key)
	}
	e.jobKeysMu.Unlock()
}

// updateDownloadJob commits catalog-aware tracker state before job emission.
// Rejected generation/terminal writes cannot announce a physical success.
func (e *DeploymentsExecutor) updateDownloadJob(a *downloadAttempt, key, status, file string, done, total int, progress float64, downloaded, size, speed int64) {
	if a.ctx.Err() != nil {
		status, speed = string(constants.StatusCancelled), 0
	}
	if modelregistry.IsTerminalStatus(status) {
		defer a.cancel()
	}
	err := e.downloads.UpdateDownloadFullGen(key, a.generation, status, file, done, total, progress, downloaded, size, speed)
	if err != nil && !errors.Is(err, modelregistry.ErrDownloadInactive) {
		status, file = string(constants.StatusFailed), err.Error()
		err = e.downloads.UpdateDownloadFullGen(key, a.generation, status, file, done, total, progress, downloaded, size, speed)
	}
	if err != nil {
		if a.ctx.Err() != nil {
			a.job.Meta(jobs.Meta{"transfer_status": string(constants.StatusCancelled), "transfer_speed": int64(0)})
		}
		a.cancel()
		a.job.Fail(fmt.Errorf("download update rejected before publication: %w", err))
		e.forgetJobKey(key, a.job.ID())
		return
	}
	a.job.Meta(jobs.Meta{"transfer_status": status, "transfer_progress": progress, "transfer_speed": speed})
	e.publishJobEvent(a.job, key, status, file, progress, downloaded, size)
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
	h.Progress(pct, currentFile, bytes)
	switch status {
	case string(constants.StatusCompleted):
		h.Done()
		e.forgetJobKey(downloadKey, h.ID())
	case string(constants.StatusFailed):
		reason := "download failed"
		if currentFile != "" {
			reason = "download failed: " + currentFile
		}
		h.Fail(errors.New(reason))
		e.forgetJobKey(downloadKey, h.ID())
	case string(constants.StatusCancelled):
		reason := "download cancelled"
		if currentFile != "" {
			reason = "download cancelled: " + currentFile
		}
		h.Fail(errors.New(reason))
		e.forgetJobKey(downloadKey, h.ID())
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
	attempt := e.beginDownload(c, downloadKey, modelName, constants.RepoOllama)
	if attempt == nil {
		return
	}
	job, ctx := attempt.job, attempt.ctx

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("PANIC recovered in Ollama download", "panic", r, "stack", string(debug.Stack()))
				e.updateDownloadJob(attempt, downloadKey, string(constants.StatusFailed), fmt.Sprintf("panic: %v", r), 0, 0, 0, 0, 0, 0)
			}
		}()

		// 48-hour safety timeout prevents orphaned contexts if StopDownload is
		// never called. Normal downloads complete much faster or are cancelled.

		statusUpdater := func(status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64) {
			e.updateDownloadJob(attempt, downloadKey, status, currentFile, filesDownloaded, filesTotal, progress, downloaded, totalSize, speed)
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
	result.JobID = job.ID()
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

	attempt := e.beginDownload(c, downloadKey, modelName, constants.RepoHuggingFace)
	if attempt == nil {
		return
	}
	job, ctx := attempt.job, attempt.ctx

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("PANIC recovered in HuggingFace download", "panic", r, "stack", string(debug.Stack()))
				e.updateDownloadJob(attempt, downloadKey, string(constants.StatusFailed), fmt.Sprintf("panic: %v", r), 0, 0, 0, 0, 0, 0)
			}
		}()

		statusUpdater := func(status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64) {
			if status == string(constants.StatusCompleted) && provider != "" && e.ensureFeatures != nil {
				root, err := modelregistry.GetModelsRootDir()
				if err == nil {
					err = e.ensureFeatures(ctx, provider, filepath.Join(root, request.Repo), features, request.Optional, func(message string) {
						job.Progress(99, message, jobs.Bytes{Done: downloaded, Total: totalSize})
					})
				}
				if err != nil {
					status, currentFile = string(constants.StatusFailed), err.Error()
				}
			}
			e.updateDownloadJob(attempt, downloadKey, status, currentFile, filesDownloaded, filesTotal, progress, downloaded, totalSize, speed)
		}

		e.registry.DownloadHuggingFaceModelAsync(ctx, request, statusUpdater)
	}()

	result := e.newDeploymentResult(
		downloadKey, modelName, constants.RepoHuggingFace,
		string(constants.StatusDownloading),
		fmt.Sprintf("Download initiated for huggingface/%s", modelName),
	)
	result.JobID = job.ID()
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

	if !publishOr503(c, e.publishCatalog) {
		return
	}

	slog.Info("Cloud model registered", "provider", providerName, "model", modelName)

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
