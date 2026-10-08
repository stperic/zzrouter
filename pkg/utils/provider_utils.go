package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
)

// ============================================================================
// Provider Type Normalization
// ============================================================================

// NormalizeAppType normalizes provider type names to official standard format (case-insensitive)
func NormalizeAppType(providerType string) string {
	lower := strings.ToLower(providerType)
	switch lower {
	case constants.AppOllama:
		return constants.AppOllama
	default:
		return lower
	}
}

// ============================================================================
// Node Name Standardization (DRY - Single Source of Truth)
// ============================================================================

// ============================================================================
// HTTP Utilities (DRY - Single Source of Truth)
// ============================================================================

// HTTPResponse wraps common HTTP response handling
type HTTPResponse struct {
	Body       []byte
	StatusCode int
}

// MakeHTTPRequestWithContext performs an HTTP request with a context and standard error handling
func MakeHTTPRequestWithContext(ctx context.Context, client *http.Client, method, url string, body any) (*HTTPResponse, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return &HTTPResponse{
		Body:       responseBody,
		StatusCode: resp.StatusCode,
	}, nil
}

// DecodeJSONResponse decodes JSON response with standard error handling
func DecodeJSONResponse(responseBody []byte, target any) error {
	if err := json.Unmarshal(responseBody, target); err != nil {
		return fmt.Errorf("failed to decode JSON response: %w", err)
	}
	return nil
}

// CheckHTTPStatus validates HTTP status code with standard error handling
func CheckHTTPStatus(statusCode int, responseBody []byte, operation string) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}

	// Try to extract error message from response body
	var errorResp map[string]any
	if json.Unmarshal(responseBody, &errorResp) == nil {
		if errMsg, ok := errorResp["error"].(string); ok {
			return fmt.Errorf("%s failed with status %d: %s", operation, statusCode, errMsg)
		}
	}

	return fmt.Errorf("%s failed with status %d", operation, statusCode)
}
