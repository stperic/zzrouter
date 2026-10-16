package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ============================================================================
// Shared Helper Functions - Self-Contained Utilities
// ============================================================================
//
// These helpers are used throughout the server module.
// They are pure functions that can be used across the codebase.

// ============================================================================
// Context Helpers - Standard Timeout Patterns
// ============================================================================

// Common timeout durations for different operation types
// These are aliases to the centralized constants in pkg/constants/timeouts.go
const (
	// DefaultRequestTimeout is the standard timeout for most API requests
	DefaultRequestTimeout = constants.HTTPDefaultTimeout
	// LongRequestTimeout is for operations that may take longer (restarts, config changes)
	LongRequestTimeout = constants.HTTPLongTimeout
)

// ensureTimeout adds a timeout to ctx if it doesn't already have a deadline.
// Returns a no-op cancel func when the context already has a deadline.
// Use in service methods to guarantee a timeout without double-wrapping.
func ensureTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// RoutedError preserves the HTTP status code and parsed Problem Details from a
// routed response. When a public handler routes to an internal endpoint and the
// internal endpoint returns an RFC 9457 Problem Details JSON, we parse it eagerly
// and attach it here so the outer handler can forward the inner error faithfully
// instead of stringifying it into a new error's detail field (which used to
// produce the infamous double-escaped JSON in error responses).
//
// Usage in controllers:
//
//	if err != nil {
//	    RespondToError(c, err)  // handles *RoutedError transparently
//	    return
//	}
type RoutedError struct {
	StatusCode     int
	RetryAfter     string // Preserves admission backoff across worker/coordinator hops.
	Body           string
	ProblemDetails *utils.ProblemDetails // parsed inner Problem Details, if any
}

// Error returns the detail from the parsed Problem Details when available,
// otherwise a concise fallback. It deliberately does NOT embed the full response
// body; callers that need the structured error should type-assert to
// *RoutedError or use RespondToError to forward it.
func (e *RoutedError) Error() string {
	if e.ProblemDetails != nil && e.ProblemDetails.Detail != "" {
		return e.ProblemDetails.Detail
	}
	if e.ProblemDetails != nil && e.ProblemDetails.Title != "" {
		return e.ProblemDetails.Title
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

func (e *RoutedError) setRetryAfter(c *gin.Context) {
	if e.RetryAfter != "" {
		c.Header("Retry-After", e.RetryAfter)
	}
}

// Respond forwards the inner error to the client, preserving its status code
// and Problem Details structure. A fresh request_id and instance path are set
// from the current request so operators can correlate logs across hops.
func (e *RoutedError) Respond(c *gin.Context) {
	e.setRetryAfter(c)
	if httperr.FromContextOr(c, defaultAuthResponder).Name() != "problem" {
		wire := httperr.Error{Status: e.StatusCode, Message: e.Error()}
		if pd := e.ProblemDetails; pd != nil {
			wire.Code, wire.Title = pd.Code, pd.Title
			wire.Extra = map[string]any{}
			if pd.Deleted != nil {
				wire.Extra["deleted"] = *pd.Deleted
			}
			if pd.MutationApplied {
				wire.Extra["mutation_applied"] = true
			}
			if pd.RunID != "" {
				wire.Extra["run_id"] = pd.RunID
			}
			if pd.Node != "" {
				wire.Extra["node"] = pd.Node
			}
			if len(pd.Errors) > 0 {
				wire.Extra["errors"] = pd.Errors
			}
		}
		respondSurfaceError(c, wire)
		return
	}
	if e.ProblemDetails != nil {
		pd := *e.ProblemDetails // copy by value so we don't mutate the shared error
		pd.Instance = c.Request.URL.Path
		pd.RequestID = ""
		if reqID, ok := c.Get("request_id"); ok {
			if id, ok := reqID.(string); ok {
				pd.RequestID = id
			}
		}
		c.Header("Content-Type", "application/problem+json")
		c.JSON(e.StatusCode, pd)
		return
	}
	// Fallback for non-Problem-Details bodies (e.g. HTML error pages).
	RespondWithProblem(c, e.StatusCode, http.StatusText(e.StatusCode), utils.SanitizeErrorMessage(e.Body))
}

// newProblemError constructs a *RoutedError carrying a synthetic Problem
// Details payload. Service-layer code uses this to surface structured errors
// with a specific HTTP status code without having to go through a real routed
// HTTP call. Example:
//
//	return newProblemError(http.StatusBadGateway, "Bad Gateway", "...")
//
// RespondToError will forward the Problem Details verbatim, preserving the
// status code and the detail string.
func newProblemError(status int, title, detail string) *RoutedError {
	pd := utils.NewProblemDetails(status, title, detail, "")
	return &RoutedError{
		StatusCode:     status,
		ProblemDetails: pd,
	}
}

// parseRoutedError builds a *RoutedError from a routed response body,
// attempting to parse the body as a Problem Details JSON. If parsing succeeds
// and the result looks like a Problem Details (has a Title), it is attached.
func parseRoutedError(statusCode int, body []byte, headers http.Header) *RoutedError {
	re := &RoutedError{StatusCode: statusCode, Body: string(body), RetryAfter: headers.Get("Retry-After")}
	if len(body) == 0 {
		return re
	}
	var pd utils.ProblemDetails
	if err := json.Unmarshal(body, &pd); err == nil && pd.Title != "" {
		defaults := utils.NewProblemDetails(statusCode, http.StatusText(statusCode), "", "")
		pd.Status = statusCode
		if pd.Type == "" {
			pd.Type = defaults.Type
		}
		if pd.Code == "" {
			pd.Code = defaults.Code
		}
		re.ProblemDetails = &pd
	}
	return re
}

// routeAndParse builds a routing.Request, sends it via router.Route, and unmarshals the response body.
//
// Error handling is deliberate: every failure path returns a typed
// *RoutedError with a status code so callers can forward the error
// through RespondToError / the dialect-aware responder chain without
// stringifying anything themselves. The previous implementation used
// fmt.Errorf("%w") for routing and unmarshal errors, which caused the
// underlying Go error text — including internal type names like
// `types.InstanceStatus` from json.Unmarshal — to reach the client
// verbatim. The dialect-aware responders and the openaicompat
// sanitizer would now strip those at the edge, but fixing the root
// cause is still the right change: it removes an entire class of
// controller-level err.Error() passthrough leaks and gives callers a
// stable status code to branch on.
//
// Returned RoutedError shapes:
//
//   - Routing transport failure (router.Route returned err)   → 502
//   - Upstream responded with 4xx/5xx                         → verbatim status
//   - Successful transport but body did not match the expected
//     Go shape                                                → 502
//
// The 502 choice for both transport and shape failures is intentional:
// from a client's perspective, anything where the upstream could not
// be reached or understood is an upstream (gateway) problem, not a
// zzRouter-internal crash (which would warrant 500).
func routeAndParse[T any](ctx context.Context, router routing.Router, method, path, node string, body []byte) (*T, error) {
	resp, err := router.Route(ctx, &routing.Request{
		Path:   path,
		Method: method,
		Node:   node,
		Body:   body,
	})
	if err != nil {
		// Log the raw error at debug level for operators; the
		// RoutedError carries only a sanitized summary.
		utils.LogDebugf("[routeAndParse] transport error: %v (method=%s path=%s node=%s)", err, method, path, node)
		return nil, newRoutedTransportError(err, node)
	}
	if resp.StatusCode >= 400 {
		return nil, parseRoutedError(resp.StatusCode, resp.Body, resp.Headers)
	}
	var result T
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		// The json package's error text carries Go type names
		// (`types.InstanceStatus`) and field paths that are
		// infrastructure-leaky. Log them at debug level for
		// operator correlation and return a sanitized 502 so the
		// client sees an upstream-shape error instead of internal
		// Go type layout.
		utils.LogDebugf("[routeAndParse] unmarshal error: %v (method=%s path=%s status=%d)", err, method, path, resp.StatusCode)
		return nil, newRouteProblemError(http.StatusBadGateway, "Bad Gateway",
			"upstream returned a response this endpoint could not parse, which usually means peer version skew",
			httperr.CodeUpstreamShape, node)
	}
	return &result, nil
}

// RequestContext creates a context with timeout from a gin.Context for standard operations.
// This eliminates the boilerplate of `ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)`
// followed by `defer cancel()`.
//
// ============================================================================
// Cloud Provider Helpers
// ============================================================================

// isCloudProvider checks if the given app name corresponds to a cloud provider in provider config
// routeAndRelay dispatches a request to a node and writes the upstream
// response through unchanged.
//
// Its sibling routeAndParse decodes into a Go shape; use this one when the
// endpoint's body IS the contract and this layer has no opinion about it.
// Relaying verbatim is what keeps a routed read honest -- a DTO invented
// here would silently drop whatever field the upstream learned to send
// next, which is the same class of quiet wrongness as not routing at all.
//
// Error handling is routeAndParse's, minus the shape check it has no
// business making.
func routeAndRelay(c *gin.Context, router routing.Router, method, path, node string, body []byte) {
	resp, err := router.Route(c.Request.Context(), &routing.Request{
		Path:   path,
		Method: method,
		Node:   node,
		Body:   body,
	})
	if err != nil {
		utils.LogDebugf("[routeAndRelay] transport error: %v (method=%s path=%s node=%s)", err, method, path, node)
		newRoutedTransportError(err, node).Respond(c)
		return
	}
	if resp.StatusCode >= 400 {
		parseRoutedError(resp.StatusCode, resp.Body, resp.Headers).Respond(c)
		return
	}
	c.Data(resp.StatusCode, "application/json", resp.Body)
}

func isCloudProvider(appsConfig *pkgConfig.AppsConfig, appName string) bool {
	if appName == "" {
		return false
	}
	cp := appsConfig.GetCloud(appName)
	return cp != nil
}

// ============================================================================
// Provider Configuration Helpers
// ============================================================================

// isLocalNodeCompatible shares storage eligibility with deployment planning.
func (s *Server) isLocalNodeCompatible(model, repo, provider string) bool {
	if s.model.Cache.HasModel(model) {
		return true
	}
	return len(s.eligibleDeployProviders(model, repo, "", provider, false)) > 0
}

// buildIncompatibleDeployError builds a descriptive error message explaining why a
// deploy was blocked: what format was detected, which apps exist on the node, and
// what formats they support.
func (s *Server) buildIncompatibleDeployError(hostname, model, repo string) string {
	// Report the same providers the compatibility check consulted:
	// listing merely-configured ones would tell the operator the node has
	// a provider that could serve this model when it has no such thing.
	var providers []modelregistry.ProviderFormatEntry
	if s.providers.appMgr != nil {
		for _, info := range s.buildCompatibleAppsInfo() {
			providers = append(providers, modelregistry.ProviderFormatEntry{
				Name:    info.Key,
				Formats: info.Formats,
			})
		}
	}

	detectedFormat := modelregistry.DetectFormatFromName(model)
	msg := modelregistry.FormatIncompatibleError(model, detectedFormat, repo, providers)
	return fmt.Sprintf("cannot deploy to node %q: %s, or use --force to override", hostname, msg)
}

// RouteToModelOrBroadcast uses the same target selection as inference and preview tiers.
func (s *Server) RouteToModelOrBroadcast(ctx context.Context, modelName, path, method string, body []byte) (*routing.Response, error) {
	resolved, err := s.ResolveModel(ctx, resolver.Reference{Model: modelName})
	if err != nil {
		return nil, err
	}
	node := resolved.Node
	if node == "" && resolved.Provider != "" && !s.node.IsWorker() {
		node = s.node.Nodename()
	}
	if node != "" {
		return s.cluster.router.Unicast(ctx, node, path, method, body)
	}
	return s.cluster.router.Broadcast(ctx, path, method, body)
}

// ResolveModel shares group-first resolution with inference and management callers.
func (s *Server) ResolveModel(ctx context.Context, ref resolver.Reference) (*resolver.Resolved, error) {
	return s.model.Resolver.Resolve(ctx, ref)
}

// findMatchingModels finds models that match a name pattern (fuzzy matching)
func findMatchingModels(pattern string, models []*metadata.ModelMetadata) []*metadata.ModelMetadata {
	if pattern == "" {
		return models
	}

	matches := []*metadata.ModelMetadata{}
	patternLower := strings.ToLower(pattern)

	for _, model := range models {
		modelNameLower := strings.ToLower(model.Name)
		// Exact match or contains
		if modelNameLower == patternLower || strings.Contains(modelNameLower, patternLower) {
			matches = append(matches, model)
		}
	}

	return matches
}
