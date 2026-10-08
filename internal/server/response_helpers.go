// package server provides HTTP handlers for the zzrouter host server.
// Admin handlers - common admin functionality and response helpers

package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/version"
)

// API Response helpers for consistent responses
// ============================================

// SuccessResponse is the standard envelope for non-list success responses.
// Data is always nested under `data` — no merging, no flattening. When data
// is nil (acknowledgement-only responses) the field is omitted.
type SuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// respondSuccess sends a standard success envelope: {success, message, data}.
// Callers that want to return multiple top-level fields should pass a struct
// or map; clients read them under the `data` key.
func respondSuccess(c *gin.Context, message string, data any) {
	c.JSON(http.StatusOK, SuccessResponse{
		Success: true,
		Message: message,
		Data:    data,
	})
}

// respondRetrievalSuccess sends a consistent success response for data retrieval operations
func respondRetrievalSuccess(c *gin.Context, resourceType string, data any) {
	message := fmt.Sprintf("%s retrieved successfully", resourceType)
	respondSuccess(c, message, data)
}

// ============================================================================
// Standard List Response Helpers
// ============================================================================
// These helpers provide consistent response envelopes for list endpoints.
// All list endpoints should use these for API consistency.

// ListResponse is a standard envelope for list responses
// Provides consistent structure across all list endpoints
// has_more is always emitted, never omitted: a client branching on
// "is there another page" should not have to tell absent from false.
// That is the same reason the model capability dict emits every flag.
type ListResponse struct {
	Data     any            `json:"data"`
	Total    int            `json:"total"`
	HasMore  bool           `json:"has_more"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// respondList sends a standard list response with consistent envelope
// Use for all list endpoints (/zzrouter/providers, /zzrouter/runs, /zzrouter/models, etc.)
func respondList(c *gin.Context, data any, total int, hasMore bool) {
	c.JSON(http.StatusOK, ListResponse{
		Data:    data,
		Total:   total,
		HasMore: hasMore,
	})
}

// respondListWithMetadata is like respondList but includes an optional metadata
// map for endpoint-specific extra fields (e.g. route_prefix for model-groups).
// Clients read these under `metadata.<key>`.
func respondListWithMetadata(c *gin.Context, data any, total int, hasMore bool, metadata map[string]any) {
	c.JSON(http.StatusOK, ListResponse{
		Data:     data,
		Total:    total,
		HasMore:  hasMore,
		Metadata: metadata,
	})
}

// ============================================================================
// Pagination Support
// ============================================================================
// Standard pagination parameters for list endpoints.
// Use ParsePagination() to extract from query params.

// Pagination constants
const (
	DefaultPageLimit = 100  // Default number of items per page
	MaxPageLimit     = 1000 // Maximum allowed items per page
)

// PaginationParams holds pagination parameters for list requests
type PaginationParams struct {
	Limit  int `form:"limit"`  // Number of items to return (default: 100, max: 1000)
	Offset int `form:"offset"` // Number of items to skip (default: 0)
}

// ParsePagination extracts pagination parameters from the query string.
// Absent parameters take their defaults; a present but unusable one is a
// 400, because silently substituting the default hands back a page the
// caller did not ask for and cannot tell apart from the one it did.
// Returns false when it has written the error response.
//
// limit is clamped to MaxPageLimit rather than rejected: asking for more
// than the cap is a reasonable "give me everything", and the applied
// value is echoed in the response metadata.
func ParsePagination(c *gin.Context) (PaginationParams, bool) {
	params := PaginationParams{
		Limit:  DefaultPageLimit,
		Offset: 0,
	}

	if limitStr := c.Query("limit"); limitStr != "" {
		limit, err := parseInt(limitStr)
		if err != nil || limit < 1 {
			BadRequest(c, fmt.Sprintf("limit must be a positive integer, got %q", limitStr))
			return params, false
		}
		params.Limit = min(limit, MaxPageLimit)
	}

	if offsetStr := c.Query("offset"); offsetStr != "" {
		offset, err := parseInt(offsetStr)
		if err != nil || offset < 0 {
			BadRequest(c, fmt.Sprintf("offset must be a non-negative integer, got %q", offsetStr))
			return params, false
		}
		params.Offset = offset
	}

	return params, true
}

// parseInt parses a query param as a base-10 integer. Sscanf would accept
// trailing junk ("10abc" -> 10), which is exactly the silent
// misinterpretation the 400 above exists to prevent.
func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
}

// ApplyPagination applies pagination to a slice and returns (paginatedData, totalCount, hasMore)
// Generic helper that works with any slice type via reflection
func ApplyPagination[T any](items []T, params PaginationParams) ([]T, int, bool) {
	total := len(items)

	// Apply offset
	start := min(params.Offset, total)

	// Apply limit
	end := min(start+params.Limit, total)

	hasMore := end < total
	result := items[start:end]
	// Ensure non-nil slice so JSON serializes as [] not null
	if result == nil {
		result = make([]T, 0)
	}
	return result, total, hasMore
}

// ============================================================================
// Standard Query Parameter Helpers
// ============================================================================
// Standardized parameter names for API consistency.

// QueryRegistry reads the repository parameter
func QueryRegistry(c *gin.Context) string {
	return c.Query("registry")
}

// QueryProvider reads the provider parameter
func QueryProvider(c *gin.Context) string {
	return c.Query("provider")
}

// QueryModel reads the model parameter. Accepts any identifier the cluster
// knows: canonical model name, provider-qualified name, or registry id.
// `name=` is accepted as a fallback for handler symmetry — most other
// admin endpoints take `?name=` for the same kind of identifier, so
// rejecting it here on /models/show gave agents three places to look
// (path :id, ?model=, ?name=) for one concept. `model=` wins on
// conflict; the precedence is non-load-bearing in practice but
// explicit beats silent.
func QueryModel(c *gin.Context) string {
	if v := c.Query("model"); v != "" {
		return v
	}
	return c.Query("name")
}

// QueryNode reads the node parameter. Accepts any identifier the coordinator
// knows about: the local node name, a configured cluster endpoint
// ("host:port"), or just the hostname portion of one.
func QueryNode(c *gin.Context) string {
	return c.Query("node")
}

// respondCreatedWithMessage sends a 201 Created response with success envelope
// Used for endpoints that follow the success envelope pattern but create resources
func respondCreatedWithMessage(c *gin.Context, message string, data any) {
	c.JSON(http.StatusCreated, SuccessResponse{
		Success: true,
		Message: message,
		Data:    data,
	})
}

// respondAccepted sends a 202 Accepted response with the success envelope.
// Use for endpoints that kick off async work and want to acknowledge the
// request without implying the work has finished.
func respondAccepted(c *gin.Context, message string, data any) {
	c.JSON(http.StatusAccepted, SuccessResponse{
		Success: true,
		Message: message,
		Data:    data,
	})
}

// respondNoContent sends a 204 No Content response for successful deletions
func respondNoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Version endpoints for cluster host validation
// ==========================================

// handleGetVersion returns comprehensive version information
func handleGetVersion(c *gin.Context) {
	versionInfo := version.GetCurrentVersionInfo(version.ServiceTypeHost, []string{
		version.CapabilityCluster, version.CapabilityAdminAPI, version.CapabilityApps, version.CapabilityOpenAIAPI, version.CapabilityClusterUpdates,
	})

	c.JSON(http.StatusOK, versionInfo)
}

// handleGetVersionSimple returns minimal version for quick checks.
// Wrapped in the canonical {success, message, data} envelope so agents
// don't have to learn a per-endpoint shape.
func handleGetVersionSimple(c *gin.Context) {
	respondSuccess(c, "Version retrieved", gin.H{
		"version":     version.Current.String(),
		"api_version": "v1",
		"service":     version.ServiceTypeHost,
		"healthy":     true,
	})
}

// handleGetCompatibility returns compatibility matrix information.
// Same envelope as /server/version — singular admin endpoints all use
// {success, message, data}.
func handleGetCompatibility(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{
		OptionalQueryParamWithDefault("type", "cluster"),
	})
	if params == nil {
		return
	}

	connectionType := params.GetString("type")
	matrix := version.GetCompatibilityMatrix(connectionType)

	respondSuccess(c, "Compatibility matrix retrieved", gin.H{
		"connection_type":       connectionType,
		"current_version":       version.Current.String(),
		"compatibility_matrix":  matrix,
		"supported_connections": []string{"cluster", "client", "admin"},
	})
}
