// package server provides HTTP handlers for the zzrouter host server.
// Utility functions - helper functions for host validation and connectivity

package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// Note: Cluster host validation is now handled by the cluster coordinator
// The cluster coordinator creates a single connector and validates all hosts during initialization
// This eliminates duplicate connection attempts and provides a single source of truth

// trackProviderRateLimit extracts rate limit headers from a provider response
// and stores the snapshot for the providers/status API.
func (s *Server) trackProviderRateLimit(provider string, resp *http.Response) {
	if s.providers.rateTracker == nil || resp == nil {
		return
	}
	if snap := fallback.ParseRateLimitHeaders(resp.Header); snap != nil {
		s.providers.rateTracker.Update(provider, *snap)
	}
}

// resolveNodeToEndpoint resolves a hostname to IP:port from cluster registry
// This is the SINGLE SOURCE OF TRUTH for hostname→IP resolution (DRY principle)
// Returns IP:port (e.g., "10.2.1.107:8080") or empty string if not found
func (s *Server) resolveNodeToEndpoint(hostname string) string {
	if s.cluster.coordinator == nil {
		return ""
	}

	endpoints := s.cluster.coordinator.GetAllEndpoints()
	for _, ep := range endpoints {
		// Match by hostname or endpoint URL (NOT IsLocal - that would always match master)
		if ep.Name == hostname || strings.Contains(ep.URL, hostname) {
			// Extract host:port from URL (e.g., "http://10.2.1.107:8080" -> "10.2.1.107:8080")
			if u, err := url.Parse(ep.URL); err == nil {
				return u.Host // Returns "IP:port"
			}
		}
	}
	return ""
}

// coordWorkerMTLSClient returns the lazily-initialized mTLS *http.Client
// the coordinator uses to dial worker cluster mTLS ports. Cached on
// first SUCCESS — failures (listener not yet wired, DialClient errors
// during early-boot pairing window) retry on subsequent calls so a
// transient race doesn't permanently disable inference dispatch.
//
// Cert-rotation note: the underlying tls.Config bakes the identity cert
// at DialClient time, NOT lazily on each handshake. A cert renewal does
// NOT propagate to outbound dispatches without a process restart. The
// inbound listener side rotates via tls.Config.GetCertificate; we
// accept the asymmetry today and revisit when rotation lands as a
// hot-swap concern (P1 from cold review, deferred).
//
// Returns nil when this server has no cluster listener — callers must
// handle nil before dispatch.
func (s *Server) coordWorkerMTLSClient() *http.Client {
	s.clusterMTLSMu.Lock()
	defer s.clusterMTLSMu.Unlock()
	if s.clusterMTLSClient != nil {
		return s.clusterMTLSClient
	}
	if s.cluster.listener == nil {
		return nil
	}
	client, err := s.cluster.listener.DialClient(clusterid.RoleWorker.OU())
	if err != nil {
		slog.Warn("coord mTLS client init failed", "error", err)
		return nil
	}
	s.clusterMTLSClient = client
	return s.clusterMTLSClient
}

// resolveNodeToClusterURL returns the mTLS cluster-port URL for a peer
// (e.g., "https://192.0.2.10:9091"), or empty when the peer is unknown.
// Falls back to DeriveClusterURL when the registry hasn't populated
// ClusterURL yet — early-boot endpoints can be registered before the
// connector hydrates their cluster port. Same fallback pattern as
// jobs_controller.go.
func (s *Server) resolveNodeToClusterURL(hostname string) string {
	if s.cluster.coordinator == nil {
		return ""
	}
	endpoints := s.cluster.coordinator.GetAllEndpoints()
	for _, ep := range endpoints {
		if ep.Name == hostname || strings.Contains(ep.URL, hostname) {
			if ep.ClusterURL != "" {
				return ep.ClusterURL
			}
			if s.cluster.coordinator != nil && s.config != nil {
				if derived, err := mesh.DeriveClusterURL(ep.URL, s.config.Cluster.BindPort); err == nil {
					return derived
				}
			}
			return ""
		}
	}
	return ""
}

// resolveEndpointToNodename resolves an IP:port to hostname from cluster registry
// This is the reverse of resolveNodeToEndpoint - used for API responses
// Returns hostname (e.g., "gpu-server") or empty string if not found
func (s *Server) resolveEndpointToNodename(endpoint string) string {
	if s.cluster.coordinator == nil {
		return ""
	}

	endpoints := s.cluster.coordinator.GetAllEndpoints()
	for _, ep := range endpoints {
		// Extract host:port from endpoint URL
		if u, err := url.Parse(ep.URL); err == nil {
			epNode := u.Host // "IP:port"
			// Match by IP:port (with or without port)
			if epNode == endpoint || u.Hostname() == endpoint {
				// Prefer NodeName if available, otherwise use Name
				if ep.NodeName != "" {
					return ep.NodeName
				}
				if ep.Name != "" {
					return ep.Name
				}
			}
			// Also try matching just the IP part
			if strings.Contains(endpoint, ":") {
				parts := strings.Split(endpoint, ":")
				if len(parts) == 2 && u.Hostname() == parts[0] {
					if ep.NodeName != "" {
						return ep.NodeName + ":" + parts[1]
					}
					if ep.Name != "" {
						return ep.Name + ":" + parts[1]
					}
				}
			}
		}
	}
	return ""
}

// routeToClusterNode routes a request to a specific cluster host using unicast
// This is a DRY helper used by all handlers that need to route to remote hosts
func (s *Server) routeToClusterNode(ctx context.Context, targetNode, path, method string, body []byte, headers http.Header) (*mesh.Response, error) {
	if s.cluster.coordinator == nil {
		return nil, fmt.Errorf("cluster  not configured")
	}

	// Clone headers to avoid modifying the original
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}

	headers.Set(clusternode.ClusterInternalHeader, "true")

	clusterReq := &mesh.Request{
		Method:     method,
		Path:       path,
		Query:      "",
		Body:       body,
		Headers:    headers,
		TargetNode: targetNode,
		Strategy:   mesh.StrategyUnicast,
	}

	return s.cluster.coordinator.HandleRequest(ctx, clusterReq)
}

// getClusterNodeURL resolves a cluster node name to its full endpoint URL.
func (s *Server) getClusterNodeURL(nodeName string) (string, error) {
	// Resolve node name to IP:port using DRY helper
	endpoint := s.resolveNodeToEndpoint(nodeName)
	if endpoint == "" {
		slog.Info("Cluster node not found in cluster registry", "node_name", nodeName)
		return "", fmt.Errorf("cluster node '%s' not found", nodeName)
	}

	fullURL := fmt.Sprintf("%s://%s", mesh.Scheme(s.config.Node.IsTLSEnabled()), endpoint)
	slog.Info("Resolved cluster node ->", "node_name", nodeName, "full_url", fullURL)
	return fullURL, nil
}

// getClusterNodeMTLSURL returns the worker's cluster mTLS port URL
// for coord→worker dispatch. Used by Ollama pull-route proxying and
// any other coord-side path that targets a worker compat surface.
func (s *Server) getClusterNodeMTLSURL(nodeName string) (string, error) {
	url := s.resolveNodeToClusterURL(nodeName)
	if url == "" {
		return "", fmt.Errorf("cluster node '%s' has no cluster URL", nodeName)
	}
	return url, nil
}

// getMTLSClient is a public-name shim over coordWorkerMTLSClient for
// callers passing the function as a constructor parameter.
func (s *Server) getMTLSClient() *http.Client {
	return s.coordWorkerMTLSClient()
}

// parseModelIdentifier is a helper to parse model identifiers with error handling.
// This is a DRY helper used by handlers that need to parse model notation without
// a registry hint or AppsConfig (no cloud-awareness).
func parseModelIdentifier(input string) (*modelregistry.Identifier, error) {
	id, err := modelregistry.Parse(input, modelregistry.ParseOpts{})
	if err != nil {
		return nil, fmt.Errorf("failed to parse model identifier: %w", err)
	}
	return id, nil
}

// splitModelRef separates a model reference into the identity the registry
// knows and the optional @node routing hint the caller decorated it with.
// Uses AppsConfig so cloud model IDs (which reuse @ and : with different
// semantics) stay opaque.
func (s *Server) splitModelRef(modelName string) (identity, node string) {
	id, err := modelregistry.Parse(modelName, modelregistry.ParseOpts{Config: s.appsConfig})
	if err != nil || id == nil {
		return modelName, ""
	}
	identity = id.GetFullModelName()
	if identity == "" {
		identity = modelName
	}
	return identity, id.Node
}

// modelIdentity is the control.ModelIdentity the access layer enforces on:
// it drops the @node decoration the catalog publishes so an allow list
// populated from /v1/models matches what the dispatcher asks about.
func (s *Server) modelIdentity(modelName string) string {
	identity, _ := s.splitModelRef(modelName)
	return identity
}

// parseInferenceModel extracts an optional @node hint from the request's model
// string and returns the clean model name the resolver should use. The explicit
// X-Node header still wins over an embedded @node — body metadata is the
// fallback, not an override.
func (s *Server) parseInferenceModel(c *gin.Context, modelName string) (cleanModel, nodeHint string) {
	cleanModel, embedded := s.splitModelRef(modelName)

	nodeHint = c.GetHeader("X-Node")
	if nodeHint == "" {
		nodeHint = embedded
	}
	return cleanModel, nodeHint
}

// streamResponseWithFlush streams response body with immediate flushing for real-time updates
// This is the industry-standard approach for streaming responses:
// - For NDJSON (Ollama API): Decodes and flushes each JSON object immediately
// - For binary/unknown: Falls back to optimized io.Copy
// - For non-flushing writers: Uses io.Copy (automatically optimized by Go runtime)
func streamResponseWithFlush(w http.ResponseWriter, body io.Reader) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flushing support - use standard io.Copy (highly optimized)
		_, err := io.Copy(w, body)
		return err
	}

	// Try to detect if this is NDJSON by reading first few bytes
	// Most Ollama API responses are NDJSON (newline-delimited JSON)
	buf := make([]byte, 1)
	firstByte := make([]byte, 0, 1024)

	// Peek at first character to detect JSON
	n, err := body.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	if n > 0 {
		firstByte = append(firstByte, buf[0])

		// If starts with '{', likely NDJSON - use line-by-line streaming
		if buf[0] == '{' {
			return streamNDJSON(w, flusher, io.MultiReader(bytes.NewReader(firstByte), body))
		}
	}

	// Not JSON or unknown format - use io.Copy for maximum throughput
	// io.Copy uses 32KB buffer and can leverage zero-copy optimizations
	if len(firstByte) > 0 {
		// Write the peeked byte first
		if _, err := w.Write(firstByte); err != nil {
			return err
		}
		flusher.Flush()
	}

	_, err = io.Copy(w, body)
	return err
}

// streamNDJSON streams newline-delimited JSON with immediate flushing after each line
// This is the correct approach for Ollama API streaming (deploy progress, chat tokens, etc.)
// Each line is a complete JSON object that should be delivered immediately
func streamNDJSON(w io.Writer, flusher http.Flusher, body io.Reader) error {
	// Use bufio.Scanner to read line-by-line
	// This is more efficient than manual buffering and handles line boundaries correctly
	scanner := bufio.NewScanner(body)

	// Increase buffer size for large JSON objects (default 64KB, max 1MB)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		// Write the complete JSON line
		line := scanner.Bytes()
		if _, err := w.Write(line); err != nil {
			return err
		}

		// Write the newline that scanner stripped
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}

		// Flush immediately so client receives this JSON object right away
		// This is critical for real-time progress updates and chat streaming
		flusher.Flush()
	}

	return scanner.Err()
}
