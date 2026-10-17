package server

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/gin-gonic/gin"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// ============================================================================
// Shared provider helpers
// ============================================================================

// envVarRe matches ${VAR_NAME} and ${VAR_NAME:-default} to extract the variable name.
var envVarRe = regexp.MustCompile(`\$\{(\w+)(?::-[^}]*)?\}`)

// providerInstanceNameRe restricts runtime-created provider names to a safe
// filesystem-stem charset. The name becomes `<providers-dir>/external/<name>.yaml`
// via SaveToDir; path separators, dots, or uppercase letters would either
// escape the providers directory or collide with case-insensitive filesystems.
var providerInstanceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// validateProviderInstanceName returns a user-facing rejection message
// for an unusable runtime-created provider name, or "" if the name is
// fine. Charset first (filesystem safety), then reserved route
// literals (reserved_names.go).
func validateProviderInstanceName(name string) string {
	if !providerInstanceNameRe.MatchString(name) {
		return "name must match [a-z][a-z0-9-]{0,39} (lowercase, digits, hyphens)"
	}
	if reservedProviderNames[name] {
		return reservedNameMessage("name", name, "/zzrouter/v1/providers/", reservedProviderNames)
	}
	return ""
}

// extractEnvVar extracts the environment variable name from a token like "${GEMINI_API_KEY}" or "${VAR:-default}".
func extractEnvVar(token string) string {
	matches := envVarRe.FindStringSubmatch(token)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// respondFinalizeErr maps a FinalizeOnboarding/FinalizeOffboarding error
// to an HTTP response. Unknown-name errors become 404; everything else 500.
// Called from any handler whose terminal hook failed after the session's
// actual work succeeded — the prefix argument becomes the user-facing
// reason ("verify succeeded but finalize failed", etc.).
func respondFinalizeErr(c *gin.Context, prefix string, err error) {
	if errors.Is(err, pkgConfig.ErrProviderNotFound) {
		NotFound(c, fmt.Sprintf("%s: %v", prefix, err))
		return
	}
	// A worker's finalization the coordinator refused keeps that refusal.
	var refused *clustersignal.StatusError
	if errors.As(err, &refused) {
		RespondToError(c, httperr.Error{Status: refused.Status, Code: refused.Code, Message: fmt.Sprintf("%s: %v", prefix, err)})
		return
	}
	InternalNodeError(c, fmt.Sprintf("%s: %v", prefix, err))
}
