package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
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

// extractStatusCode extracts the HTTP status code from a RoutedError.
// Falls back to mapping prov_apps sentinel errors for direct (non-routed) calls.
func extractStatusCode(err error) int {
	var re *RoutedError
	if errors.As(err, &re) {
		return re.StatusCode
	}
	var memory *process.MemoryFitError
	if errors.As(err, &memory) {
		return http.StatusConflict
	}
	switch {
	case errors.Is(err, prov_apps.ErrProviderNotFound),
		errors.Is(err, prov_apps.ErrInstanceNotFound):
		return http.StatusNotFound
	case errors.Is(err, pkgConfig.ErrModelNameConflict),
		errors.Is(err, prov_apps.ErrInstallInProgress),
		errors.Is(err, prov_apps.ErrInstancesRunning),
		errors.Is(err, prov_apps.ErrProviderNotManaged),
		errors.Is(err, prov_apps.ErrProviderAlreadyInstalled),
		errors.Is(err, prov_apps.ErrInstanceAlreadyExists),
		errors.Is(err, prov_apps.ErrModelAlreadyLoaded),
		errors.Is(err, prov_apps.ErrPortConflict),
		// The weights are here but half-downloaded. Nothing the
		// caller can rephrase fixes it; they have to repair the
		// model first, which is what 409 says and 400 does not.
		errors.Is(err, prov_apps.ErrModelIncomplete):
		return http.StatusConflict
	case errors.Is(err, prov_apps.ErrUnsupportedPlatform),
		errors.Is(err, process.ErrMemoryBudgetInvalid),
		errors.Is(err, prov_apps.ErrModelNotLocal),
		errors.Is(err, prov_apps.ErrParameterValidation),
		errors.Is(err, prov_apps.ErrPortOutOfRange),
		errors.Is(err, prov_apps.ErrDangerousEnvVar):
		return http.StatusBadRequest
	case errors.Is(err, prov_apps.ErrShutdown),
		errors.Is(err, process.ErrMemoryObservation),
		errors.Is(err, prov_apps.ErrAtCapacity),
		errors.Is(err, prov_apps.ErrPortUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// respondProviderErr writes an RFC 9457 problem response for a prov_apps
// error, mapping sentinels (and RoutedError from cluster routing) to the
// right HTTP status in one shot.
func respondProviderErr(c *gin.Context, err error) {
	var memory *process.MemoryFitError
	if errors.As(err, &memory) {
		RespondWithProblemOpts(c, http.StatusConflict, "GPU memory budget cannot fit", memory.Error(), ProblemOpts{
			Code: string(httperr.CodeOutOfRange), Errors: []utils.ParamError{{
				Key: memory.Parameter, Code: string(httperr.CodeOutOfRange), Got: memory, Max: memory.FreeMiB,
				Want: "required_mib <= free_mib on each selected device", Message: memory.Error(),
				Hint: "Stop an idle zzRouter run through DELETE /runs/:id, reduce this node's budget through PATCH /providers/:name/parameters, or ask the operator to release memory held by other workloads.",
			}},
		})
		return
	}
	status := extractStatusCode(err)
	RespondWithProblem(c, status, http.StatusText(status), utils.SanitizeErrorMessage(err.Error()))
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
	InternalNodeError(c, fmt.Sprintf("%s: %v", prefix, err))
}
