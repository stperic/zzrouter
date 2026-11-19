// Ollama compat business logic. Returns status-coded *RoutedError values via
// newProblemError; the handler layer translates them to Ollama wire shape.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
)

// OllamaService defines the interface for Ollama business operations.
type OllamaService interface {
	RouteInference(c *gin.Context, endpointType string) error
	Deploy(c *gin.Context, name string, insecure bool, stream *bool) error
	ManagementCommand(c *gin.Context, endpoint string, modelLookup bool) error
	CheckBlob(c *gin.Context, digest string) error
	CreateBlob(c *gin.Context, digest string) error
	Daemon() (*backend.Resolved, error)
}

// OllamaServiceImpl implements OllamaService
type OllamaServiceImpl struct {
	ollamaDaemon          func() (*backend.Resolved, error)
	resolveModel          func(context.Context, resolver.Reference) (*resolver.Resolved, error)
	proxyToRemoteNode     func(*gin.Context, string, string, []byte)
	isLocalNode           func(string) bool
	httpStreamingClient   *http.Client
	getClusterNodeMTLSURL func(string) (string, error)
	getMTLSClient         func() *http.Client
	proxyToOllamaProvider func(*gin.Context, *backend.Resolved, string, []byte, func() error)
	httpClient            *http.Client
	hasRouter             bool

	// dispatchInference runs the shared resolver + fallback pipeline for
	// /api/chat, /api/generate, /api/embeddings, and /api/embed. The
	// endpointType arg is the Ollama route name; it threads down to the
	// inference recorder so OTel emission tags the right operation_name.
	// Returns a *RoutedError on failure; the handler layer renders it in
	// Ollama's flat error shape.
	dispatchInference func(c *gin.Context, body []byte, modelName, endpointType string) error

	publishCatalog func() error
}

// NewOllamaService creates a new OllamaService
func NewOllamaService(
	ollamaDaemon func() (*backend.Resolved, error),
	resolveModel func(context.Context, resolver.Reference) (*resolver.Resolved, error),
	proxyToRemoteNode func(*gin.Context, string, string, []byte),
	isLocalNode func(string) bool,
	httpStreamingClient *http.Client,
	getClusterNodeMTLSURL func(string) (string, error),
	getMTLSClient func() *http.Client,
	proxyToOllamaProvider func(*gin.Context, *backend.Resolved, string, []byte, func() error),
	httpClient *http.Client,
	hasRouter bool,
	dispatchInference func(c *gin.Context, body []byte, modelName, endpointType string) error,
	publishCatalog func() error,
) OllamaService {
	return &OllamaServiceImpl{
		ollamaDaemon:          ollamaDaemon,
		resolveModel:          resolveModel,
		proxyToRemoteNode:     proxyToRemoteNode,
		isLocalNode:           isLocalNode,
		httpStreamingClient:   httpStreamingClient,
		getClusterNodeMTLSURL: getClusterNodeMTLSURL,
		getMTLSClient:         getMTLSClient,
		proxyToOllamaProvider: proxyToOllamaProvider,
		httpClient:            httpClient,
		hasRouter:             hasRouter,
		dispatchInference:     dispatchInference,
		publishCatalog:        publishCatalog,
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
	if err := ollamaMutationAccessError(c); err != nil {
		return err
	}
	cleanModel, targetNode, err := s.mutationTarget(c, name)
	if err != nil {
		return err
	}
	return s.pullProxy(c, targetNode, cleanModel, insecure, stream)
}

// mutationTarget binds import operations without requiring a model to exist.
func (s *OllamaServiceImpl) mutationTarget(c *gin.Context, model string) (string, string, error) {
	if c.Request.URL.Query().Has("node") {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", "the Ollama API does not support ?node; use X-Node or model@node")
	}
	id, err := parseModelIdentifier(model)
	if err != nil {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", "invalid model identifier: "+err.Error())
	}
	cleanModel := id.GetFullModelName()
	if model != "" && (cleanModel == "" || strings.Contains(cleanModel, "@") || strings.Contains(model, "@") && id.Node == "") {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", "invalid Ollama model or node selector")
	}
	if id.ProviderKey != "" && !strings.EqualFold(id.ProviderKey, constants.AppOllama) {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", "the Ollama API cannot target a different provider")
	}
	if id.File != "" || strings.Contains(model, "#") {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", "the Ollama API does not support file selectors")
	}
	if id.Repository != "" && !strings.EqualFold(id.Repository, constants.RepoOllama) {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", fmt.Sprintf("registry %q is not supported via the Ollama API; use /zzrouter/v1/deployments for non-Ollama downloads", id.Repository))
	}
	node := strings.TrimSpace(c.GetHeader("X-Node"))
	local := func(n string) bool { return n != "" && s.isLocalNode != nil && s.isLocalNode(n) }
	if node != "" && id.Node != "" && !strings.EqualFold(node, id.Node) && !(local(node) && local(id.Node)) {
		return "", "", newProblemError(http.StatusBadRequest, "Bad Request", "X-Node conflicts with the model node selector")
	}
	if node == "" {
		node = id.Node
	}
	if local(node) {
		node = ""
	}
	return cleanModel, node, nil
}

// pullProxy uses the same streaming owner as create and blob uploads.
func (s *OllamaServiceImpl) pullProxy(c *gin.Context, node, model string, insecure bool, stream *bool) error {
	body, err := json.Marshal(OllamaPullRequest{Model: model, Insecure: insecure, Stream: stream})
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to marshal request")
	}
	return s.forwardOllama(c, node, "/api/pull", bytes.NewReader(body), int64(len(body)), s.publishCatalog)
}

// forwardOllama streams requests to the selected daemon, using mTLS for workers.
func (s *OllamaServiceImpl) forwardOllama(c *gin.Context, node, path string, body io.Reader, length int64, publish func() error) error {
	client := s.httpStreamingClient
	var target *backend.Resolved
	if node == "" {
		var err error
		target, err = s.Daemon()
		if err != nil {
			return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", err.Error())
		}
	} else {
		if !s.hasRouter {
			return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "router not configured")
		}
		endpoint, err := s.getClusterNodeMTLSURL(node)
		if err != nil || endpoint == "" {
			return newProblemError(http.StatusBadGateway, "Bad Gateway", fmt.Sprintf("cluster node %q has no reachable cluster endpoint", node))
		}
		client = s.getMTLSClient()
		if client == nil {
			return newProblemError(http.StatusBadGateway, "Bad Gateway", "coord mTLS dispatch unavailable")
		}
		target = &backend.Resolved{Endpoint: endpoint, Upstream: backend.Cluster()}
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, strings.TrimRight(target.Endpoint, "/")+path, body)
	if err != nil {
		return newProblemError(http.StatusInternalServerError, "Internal Server Error", "failed to create proxy request")
	}
	req.Header = upstreamHeaders(c.Request.Context(), c.Request.Header, target.Upstream)
	// The selected worker owns this request. Do not send a coordinator alias
	// that would select a different node when the worker handles the same API.
	req.Header.Del("X-Node")
	if path == "/api/pull" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.ContentLength = length
	resp, err := client.Do(req)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return newProblemError(http.StatusRequestEntityTooLarge, "Request Entity Too Large",
				fmt.Sprintf("request body exceeds the %d-byte limit", tooLarge.Limit))
		}
		return newProblemError(http.StatusBadGateway, "Bad Gateway", "upstream Ollama request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	return writeOllamaMutation(c, resp, publish)
}

// ManagementCommand handles Ollama management commands (create, delete, copy, show, push)
func (s *OllamaServiceImpl) ManagementCommand(c *gin.Context, endpoint string, modelLookup bool) error {
	if err := ollamaMutationAccessError(c); err != nil {
		return err
	}
	if endpoint == "/api/delete" {
		return newProblemError(400, "Bad Request", "model deletion uses the guarded model service")
	}
	var publish func() error
	switch endpoint {
	case "/api/create", "/api/copy":
		publish = s.publishCatalog
	}
	body, err := c.GetRawData()
	if err != nil {
		return newProblemError(http.StatusBadRequest, "Bad Request", "failed to read request body")
	}

	if endpoint == "/api/create" {
		var req OllamaManagementRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newProblemError(http.StatusBadRequest, "Bad Request", "invalid JSON: "+err.Error())
		}
		name := req.Model
		if name == "" {
			name = req.Name
		}
		if name == "" {
			return newProblemError(http.StatusBadRequest, "Bad Request", "model name is required")
		}
		model, node, err := s.mutationTarget(c, name)
		if err != nil {
			return err
		}
		// Keep import options opaque while removing routing from both aliases.
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		raw["model"], _ = json.Marshal(model)
		if _, ok := raw["name"]; ok {
			raw["name"] = raw["model"]
		}
		body, _ = json.Marshal(raw)
		return s.forwardOllama(c, node, endpoint, bytes.NewReader(body), int64(len(body)), publish)
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

		// Management commands share direct model selection with inference.
		resolved, err := s.resolveModel(c.Request.Context(), resolver.Reference{Model: modelName, Node: c.GetHeader("X-Node")})
		if err != nil {
			return err
		}
		if host, provider := resolved.Node, resolved.Provider; host != "" {
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
		s.proxyToOllamaProvider(c, daemon, endpoint, body, publish)
		return nil
	}

	// Direct proxy for non-model-lookup commands
	daemon, err := s.Daemon()
	if err != nil {
		return newProblemError(http.StatusServiceUnavailable, "Service Unavailable", err.Error())
	}

	slog.Info("Proxying Ollama request", "endpoint", endpoint, "ollama_url", daemon.Endpoint)
	s.proxyToOllamaProvider(c, daemon, endpoint, body, publish)
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

// CheckBlob checks for a blob on the selected node and relays its native status.
func (s *OllamaServiceImpl) CheckBlob(c *gin.Context, digest string) error {
	_, node, err := s.mutationTarget(c, "")
	if err != nil {
		return err
	}
	return s.forwardOllama(c, node, "/api/blobs/"+digest, nil, 0, nil)
}

// CreateBlob streams a blob to the selected node without buffering it.
func (s *OllamaServiceImpl) CreateBlob(c *gin.Context, digest string) error {
	if err := ollamaMutationAccessError(c); err != nil {
		return err
	}
	_, node, err := s.mutationTarget(c, "")
	if err != nil {
		return err
	}
	return s.forwardOllama(c, node, "/api/blobs/"+digest, c.Request.Body, c.Request.ContentLength, nil)
}
