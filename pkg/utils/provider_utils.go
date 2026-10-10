package utils

import (
	"encoding/json"
	"fmt"
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
