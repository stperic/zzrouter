// Ollama compat business logic. Returns status-coded *RoutedError values via
// newProblemError; the handler layer translates them to Ollama wire shape.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

// OllamaService defines the interface for Ollama business operations.
type OllamaService interface {
	RouteInference(c *gin.Context, endpointType string) error
	Deploy(c *gin.Context, name string, insecure bool, stream *bool) error
	ManagementCommand(c *gin.Context, endpoint string, modelLookup bool) error
	CheckBlob(ctx context.Context, digest string) (int, error)
	CreateBlob(c *gin.Context, digest string) error
	Daemon() (*backend.Resolved, error)
}

// OllamaServiceImpl implements OllamaService
type OllamaServiceImpl struct {
	ollamaDaemon          func() (*backend.Resolved, error)
	getRemoteModelInfo    func(string, ...string) (string, string)
	proxyToRemoteNode     func(*gin.Context, string, string, []byte)
	getNodename           func() string
	isLocalNode           func(string) bool
	httpStreamingClient   *http.Client
	getClusterNodeURL     func(string) (string, error)
	getClusterNodeMTLSURL func(string) (string, error)
	getMTLSClient         func() *http.Client
	proxyToOllamaProvider func(*gin.Context, *backend.Resolved, string, []byte)
	httpClient            *http.Client
	hasRouter             bool

	// dispatchInference runs the shared resolver + fallback pipeline for
	// /api/chat, /api/generate, /api/embeddings, and /api/embed. The
	// endpointType arg is the Ollama route name; it threads down to the
	// inference recorder so OTel emission tags the right operation_name.
	// Returns a *RoutedError on failure; the handler layer renders it in
	// Ollama's flat error shape.
	dispatchInference func(c *gin.Context, body []byte, modelName, endpointType string) error

	// invalidateModelCache forces the next ListModels call to re-scan all
	// sources (including the Ollama daemon's /api/tags). Called after a
	// successful /api/pull on both local and cluster routes so the new
	// model appears on /v1/models + /v1/chat/completions without a
	// separate /zzrouter/v1/deployments registration. The worker also
	// invalidates its own cache via pullLocalProxy on the worker side;
	// the coord-side hook is what surfaces the worker-pulled model on
	// the coord's catalog without waiting for periodic peer-sync.
	invalidateModelCache func()
}

// NewOllamaService creates a new OllamaService
func NewOllamaService(
	ollamaDaemon func() (*backend.Resolved, error),
	getRemoteModelInfo func(string, ...string) (string, string),
	proxyToRemoteNode func(*gin.Context, string, string, []byte),
	getNodename func() string,
	isLocalNode func(string) bool,
	httpStreamingClient *http.Client,
	getClusterNodeURL func(string) (string, error),
	getClusterNodeMTLSURL func(string) (string, error),
	getMTLSClient func() *http.Client,
	proxyToOllamaProvider func(*gin.Context, *backend.Resolved, string, []byte),
	httpClient *http.Client,
	hasRouter bool,
	dispatchInference func(c *gin.Context, body []byte, modelName, endpointType string) error,
	invalidateModelCache func(),
) OllamaService {
	return &OllamaServiceImpl{
		ollamaDaemon:          ollamaDaemon,
		getRemoteModelInfo:    getRemoteModelInfo,
		proxyToRemoteNode:     proxyToRemoteNode,
		getNodename:           getNodename,
		isLocalNode:           isLocalNode,
		httpStreamingClient:   httpStreamingClient,
		getClusterNodeURL:     getClusterNodeURL,
		getClusterNodeMTLSURL: getClusterNodeMTLSURL,
		getMTLSClient:         getMTLSClient,
		proxyToOllamaProvider: proxyToOllamaProvider,
		httpClient:            httpClient,
		hasRouter:             hasRouter,
		dispatchInference:     dispatchInference,
		invalidateModelCache:  invalidateModelCache,
	}
}

// Daemon returns the configured Ollama: its endpoint and what it is sent.
func (s *OllamaServiceImpl) Daemon() (*backend.Resolved, error) {
	return s.ollamaDaemon()
}

// RouteInference parses the Ollama inference body, stashes request context
// (model, original body, per-request hints), and hands off to the shared
// dispatch pipeline. Protocol-specific parsing stays here; routing, load
// balancing, and failover live in dispatchOllamaInference (shared with /v1/*).
func (s *OllamaServiceImpl) RouteInference(c *gin.Context, endpointType string) error {
	body, err := c.GetRawData()
	if err != nil {
		return newProblemError(http.StatusBadRequest, "Bad Request", "failed to read request body")
	}

	var req OllamaInferenceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return newProblemError(http.StatusBadRequest, "Bad Request", "invalid JSON: "+err.Error())
	}

	if req.Model == "" {
		return newProblemError(http.StatusBadRequest, "Bad Request", "model field is required")
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	ctx := context.WithValue(c.Request.Context(), CtxKeyModel, req.Model)
	ctx = context.WithValue(ctx, CtxKeyOriginalBody, body)
	// Parse per-request keep_alive override once at the handler edge;
	// consumers pick it off via CtxKeyRequestHints.
	ctx = context.WithValue(ctx, CtxKeyRequestHints, keepalive.Parse(body))
	c.Request = c.Request.WithContext(ctx)

	slog.Debug("Ollama inference dispatch", "endpoint_type", endpointType, "model", req.Model)
	return s.dispatchInference(c, body, req.Model, endpointType)
}

// Deploy downloads a model via the Ollama API. Non-Ollama registries
// (HuggingFace, etc.) return 400 pointing at /zzrouter/v1/deployments —
// /api/* is scoped to Ollama-managed downloads.
func (s *OllamaServiceImpl) Deploy(c *gin.Context, name string, insecure bool, stream *bool) error {
	id, err := parseModelIdentifier(name)
	if err != nil {
		return newProblemError(http.StatusBadRequest, "Bad Request", "invalid model identifier: "+err.Error())
	}

	targetNode := id.Node
	targetRepo := id.Repository
	cleanModel := id.GetFullModelName()
	if targetRepo == "" {
		targetRepo = constants.RepoOllama
	}
	if targetNode == "" {
		targetNode = s.getNodename()
	}

	slog.Info("Ollama deploy request parsed", "name", name, "node", targetNode, "registry", targetRepo, "model", cleanModel)

	if !s.isLocalNode(targetNode) {
		return s.pullClusterRoute(c, targetNode, cleanModel, insecure, stream)
	}
	if strings.ToLower(targetRepo) != constants.RepoOllama {
		return newProblemError(http.StatusBadRequest,
			"Bad Request",
			fmt.Sprintf("registry %q is not supported via the Ollama API; use /zzrouter/v1/deployments for non-Ollama downloads", targetRepo))
	}
	return s.pullLocalProxy(c, cleanModel, insecure, stream)
}

// pullLocalProxy proxies deploy request to local Ollama provider
func (s *OllamaServiceImpl) pullLocalProxy(c *gin.Context, modelName string, insecure bool, stream *bool) error {
	daemon, err := s.Daemon()
	if err != nil {
		return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", err.Error())
	}

	slog.Info("Proxying Ollama deploy request", "model_name", modelName, "ollama_url", daemon.Endpoint)

	cleanReq := OllamaPullRequest{
		Model:    modelName,
		Insecure: insecure,
		Stream:   stream,
	}

	cleanBody, err := json.Marshal(cleanReq)
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to marshal request")
	}

	proxyReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		strings.TrimRight(daemon.Endpoint, "/")+"/api/pull", bytes.NewReader(cleanBody))
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to create proxy request")
	}

	proxyReq.Header = upstreamHeaders(c.Request.Header, daemon.Upstream)
	proxyReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpStreamingClient.Do(proxyReq)
	if err != nil {
		return newProblemError(http.StatusBadGateway, "Bad Gateway", "upstream Ollama request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	writeOllamaResponse(c, resp)

	// Successful upstream pull → invalidate the model cache so the next
	// /v1/models or /v1/chat/completions sees the new entry without a
	// separate /zzrouter/v1/deployments registration. The Ollama
	// connector queries the daemon's /api/tags during re-scan, so the
	// pulled model surfaces automatically. Only fires on 2xx upstream:
	// a streamed pull error keeps the cache as-is.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && s.invalidateModelCache != nil {
		s.invalidateModelCache()
	}

	return nil
}

// pullClusterRoute routes deploy request to remote cluster host
func (s *OllamaServiceImpl) pullClusterRoute(c *gin.Context, targetNode, modelName string, insecure bool, stream *bool) error {
	if !s.hasRouter {
		return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "router not configured")
	}

	startTime := utils.Now()
	utils.LogDebugf("⏱️  [PERF] Starting cluster deploy routing for host='%s', model='%s'", targetNode, modelName)

	// Get cluster host URL
	clusterNodeURL, err := s.getClusterNodeMTLSURL(targetNode)
	if err != nil {
		return newProblemError(http.StatusBadGateway, "Bad Gateway", fmt.Sprintf("failed to resolve cluster host %q: %s", targetNode, err.Error()))
	}
	utils.LogDebugf("⏱️  [PERF] Node resolution took: %v", time.Since(startTime))

	forwardReq := OllamaPullRequest{
		Model:    modelName,
		Insecure: insecure,
		Stream:   stream,
	}

	bodyBytes, err := json.Marshal(forwardReq)
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to marshal request")
	}

	targetURL := fmt.Sprintf("%s/api/pull", clusterNodeURL)
	proxyReq, err := http.NewRequestWithContext(c.Request.Context(), "POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to create proxy request")
	}

	proxyReq.Header = upstreamHeaders(c.Request.Header, backend.Cluster())
	proxyReq.Header.Set("Content-Type", "application/json")
	// Auth: mTLS at the transport layer. Worker compat surface is on the
	// cluster port; transport-level OU=coordinator client cert IS the gate.

	utils.LogDebugf("⏱️  [PERF] Connecting to cluster host (mTLS): %s", targetURL)
	connectStart := utils.Now()

	mtlsClient := s.getMTLSClient()
	if mtlsClient == nil {
		return newProblemError(http.StatusBadGateway, "Bad Gateway", "coord mTLS dispatch unavailable")
	}
	resp, err := mtlsClient.Do(proxyReq)
	connTime := time.Since(connectStart)

	if err != nil {
		if connTime > 1*time.Second {
			utils.LogDebugf("⚠️  [PERF] SLOW CONNECTION: %v (expected <100ms)", connTime)
		}
		return newProblemError(http.StatusBadGateway, "Bad Gateway", fmt.Sprintf("failed to connect to cluster host %q: %s", targetNode, err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if connTime > 100*time.Millisecond {
		utils.LogDebugf("⚠️  [PERF] Connection took %v (expected <100ms)", connTime)
	} else {
		utils.LogDebugf("⏱️  [PERF] Connection established in: %v ✓", connTime)
	}

	utils.LogDebugf("⏱️  [PERF] Starting response stream (total setup time: %v)", time.Since(startTime))

	writeOllamaResponse(c, resp)

	// Successful cluster pull → invalidate coord's catalog so the next
	// /v1/models reflects the worker-pulled model without waiting for
	// the periodic peer-sync. The worker invalidates its own cache via
	// pullLocalProxy; this is the symmetric coord-side hook.
	// Caveat: pkg/model/cache shared-mode short-circuits the
	// cross-worker fetch on Invalidate, so on shared-mode coords this
	// call is effectively a no-op for cluster pulls and /v1/models
	// lags until shared-mode-aware sync catches up.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && s.invalidateModelCache != nil {
		s.invalidateModelCache()
	}

	utils.LogDebugf("⏱️  [PERF] Stream completed (total time: %v)", time.Since(startTime))
	return nil
}

// ManagementCommand handles Ollama management commands (create, delete, copy, show, push)
func (s *OllamaServiceImpl) ManagementCommand(c *gin.Context, endpoint string, modelLookup bool) error {
	body, err := c.GetRawData()
	if err != nil {
		return newProblemError(http.StatusBadRequest, "Bad Request", "failed to read request body")
	}

	if modelLookup {
		var req OllamaManagementRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newProblemError(http.StatusBadRequest, "Bad Request", "invalid JSON: "+err.Error())
		}

		modelName := req.ModelName()
		if modelName == "" {
			return newProblemError(http.StatusBadRequest, "Bad Request", "model name is required")
		}

		// Check cache for remote model
		if host, provider := s.getRemoteModelInfo(modelName, c.GetHeader("X-Node")); host != "" {
			slog.Info("Ollama routing: model on remote node", "endpoint", endpoint, "model_name", modelName, "node", host)
			s.proxyToRemoteNode(c, host, provider, body)
			return nil
		}

		// Local model
		daemon, ollamaErr := s.Daemon()
		if ollamaErr != nil {
			return newProblemError(http.StatusNotFound,
				"Not Found",
				fmt.Sprintf("model %q not found and Ollama is not configured", modelName))
		}
		// /api/push streams a 200 + NDJSON error envelope when the
		// manifest is missing, unlike sibling /api/delete + /api/copy
		// which surface upstream 404s naturally. Pre-flight a HEAD-style
		// /api/show so the modelLookup-404 contract is consistent across
		// all four verbs.
		if endpoint == "/api/push" {
			exists, lookupErr := s.ollamaModelExists(c.Request.Context(), daemon, modelName)
			switch {
			case lookupErr != nil:
				// Daemon stress / partial failure: fall through to the
				// upstream proxy and accept the 200+NDJSON-error degraded
				// path. Log so operators tracing intermittent reports
				// have a breadcrumb.
				slog.Debug("push pre-flight /api/show failed; falling through to upstream",
					"model", modelName, "err", lookupErr)
			case !exists:
				return newProblemError(http.StatusNotFound, "Not Found",
					fmt.Sprintf("model %q not found", modelName))
			}
		}
		s.proxyToOllamaProvider(c, daemon, endpoint, body)
		return nil
	}

	// Direct proxy for non-model-lookup commands
	daemon, err := s.Daemon()
	if err != nil {
		return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", err.Error())
	}

	slog.Info("Proxying Ollama request", "endpoint", endpoint, "ollama_url", daemon.Endpoint)
	s.proxyToOllamaProvider(c, daemon, endpoint, body)
	return nil
}

// ollamaModelExistsTimeout caps the /api/show pre-flight. /api/show
// reads the local manifest from disk so it should be sub-second on a
// healthy daemon; cap short so a stuck daemon doesn't drag the inbound
// /api/push deadline down with it.
const ollamaModelExistsTimeout = 5 * time.Second

// ollamaModelExists pre-flights a POST /api/show against the local
// Ollama daemon to determine whether a model manifest is present.
// Returns (false, nil) only on a clean 404 (model definitively absent)
// and (true, nil) only on 2xx (model present). Anything else (transport
// failure, 5xx, unexpected 4xx) returns an error so the caller can fall
// through to the upstream proxy rather than synthesize a misleading 404
// to the client.
func (s *OllamaServiceImpl) ollamaModelExists(ctx context.Context, daemon *backend.Resolved, modelName string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ollamaModelExistsTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"model": modelName})
	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/show", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		// 5xx (daemon stress) or unexpected 4xx: don't claim "not found".
		// Caller's lookupErr branch will fall through to the upstream
		// proxy, preserving the original behavior under degraded daemon.
		return false, fmt.Errorf("ollama /api/show: unexpected status %d", resp.StatusCode)
	}
}

// CheckBlob checks if a blob exists
func (s *OllamaServiceImpl) CheckBlob(ctx context.Context, digest string) (int, error) {
	daemon, err := s.Daemon()
	if err != nil {
		return http.StatusNotFound, err
	}

	req, err := daemon.NewRequest(ctx, http.MethodHead, "/api/blobs/"+digest, nil)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode, nil
}

// CreateBlob creates a blob
func (s *OllamaServiceImpl) CreateBlob(c *gin.Context, digest string) error {
	daemon, err := s.Daemon()
	if err != nil {
		return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", err.Error())
	}

	slog.Info("Proxying Ollama blob upload", "digest", digest, "ollama_url", daemon.Endpoint)

	proxyReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		strings.TrimRight(daemon.Endpoint, "/")+"/api/blobs/"+digest, c.Request.Body)
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to create proxy request")
	}

	proxyReq.Header = upstreamHeaders(c.Request.Header, daemon.Upstream)
	// The blob streams through as it arrives; its length goes with it, or
	// the reader body would go out chunked.
	proxyReq.ContentLength = c.Request.ContentLength

	resp, err := s.httpStreamingClient.Do(proxyReq)
	if err != nil {
		return newProblemError(http.StatusBadGateway, "Bad Gateway", "upstream Ollama request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	writeOllamaResponse(c, resp)

	return nil
}
