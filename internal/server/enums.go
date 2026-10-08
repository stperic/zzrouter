package server

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/inferencelog"
)

// ============================================================================
// Closed-vocabulary query parameter enums
// ============================================================================
//
// Centralizing the accepted values here means we can:
//  1. Surface them in error messages so agents don't have to guess.
//  2. Reference the same list from the OpenAPI spec via code generation or a
//     simple sync check.
//
// Rules of thumb:
//  - Empty string is always accepted (means "no filter / default behavior").
//  - Values are case-sensitive — agents should send exactly what we document.

// ProviderCatalogTypes lists the accepted values for /providers/catalog?type=.
// Empty string also accepted: no type filter.
var ProviderCatalogTypes = []string{constants.AppModeCloud, "local"}

// ProviderCatalogModes lists the accepted values for /providers/catalog?mode=.
// Empty string accepted: exclude registries.
// "registry": only registries. "all": everything.
var ProviderCatalogModes = []string{constants.AppModeRegistry, "all"}

// InferenceLogStatuses lists the accepted values for /inference-logs?status=.
// Empty string accepted: no status filter.
var InferenceLogStatuses = []string{inferencelog.StatusSuccess, inferencelog.StatusError}

// ValidateEnum rejects unknown values for a closed-vocabulary query parameter.
// When value is "" the parameter is treated as unset and accepted. When value
// is set but not in allowed, it calls BadRequest with a message listing the
// valid values and returns false — callers should return immediately.
//
// Usage:
//
//	if !ValidateEnum(c, "type", typeFilter, ProviderCatalogTypes) {
//	    return
//	}
func ValidateEnum(c *gin.Context, paramName, value string, allowed []string) bool {
	if value == "" {
		return true
	}
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	BadRequest(c, fmt.Sprintf(
		"invalid value %q for parameter %q; valid: [%s]",
		value, paramName, strings.Join(allowed, ", "),
	))
	return false
}
