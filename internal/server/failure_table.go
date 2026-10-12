package server

import (
	"errors"
	"net/http"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

// failureRow is one decided answer; an empty code leaves each surface its generic one.
type failureRow struct {
	sentinel error
	status   int
	code     string
}

// failureTable is the decision record every surface reads; error_status_guard_test.go holds it complete.
var failureTable = []failureRow{
	{config.ErrModelNameConflict, http.StatusConflict, "model_name_conflict"},
	// Half-downloaded weights: the caller must repair the model, which 409 says and 400 does not.
	{prov_apps.ErrModelIncomplete, http.StatusConflict, "model_incomplete"},

	{errAutoDetectModelNotFound, http.StatusNotFound, ""},
	{errAutoDetectMultipleProviders, http.StatusConflict, ""},
	{prov_apps.ErrProviderNotFound, http.StatusNotFound, ""},
	{prov_apps.ErrInstanceNotFound, http.StatusNotFound, ""},

	{prov_apps.ErrExternalService, http.StatusForbidden, ""},

	{prov_apps.ErrInstallInProgress, http.StatusConflict, ""},
	{prov_apps.ErrServiceDisabled, http.StatusConflict, ""},
	{install.ErrStalePlan, http.StatusConflict, ""},
	{prov_apps.ErrRunHeldByOtherProvider, http.StatusConflict, ""},
	{prov_apps.ErrInstancesRunning, http.StatusConflict, ""},
	{prov_apps.ErrProviderNotManaged, http.StatusConflict, ""},
	{prov_apps.ErrProviderAlreadyInstalled, http.StatusConflict, ""},
	{prov_apps.ErrInstanceAlreadyExists, http.StatusConflict, ""},
	{prov_apps.ErrModelAlreadyLoaded, http.StatusConflict, ""},
	{prov_apps.ErrPortConflict, http.StatusConflict, ""},

	{prov_apps.ErrUnsupportedPlatform, http.StatusBadRequest, ""},
	{process.ErrMemoryBudgetInvalid, http.StatusBadRequest, ""},
	{prov_apps.ErrModelNotLocal, http.StatusNotFound, ""},
	{prov_apps.ErrParameterValidation, http.StatusBadRequest, ""},
	{prov_apps.ErrPortOutOfRange, http.StatusBadRequest, ""},
	{prov_apps.ErrDangerousEnvVar, http.StatusBadRequest, ""},

	{prov_apps.ErrProviderUnavailable, http.StatusServiceUnavailable, "provider_unavailable"},
	// The daemon dropped the model: the next request loads it again.
	{prov_apps.ErrModelDropped, http.StatusServiceUnavailable, ""},
	{prov_apps.ErrShutdown, http.StatusServiceUnavailable, ""},
	{process.ErrMemoryObservation, http.StatusServiceUnavailable, ""},
	{prov_apps.ErrAtCapacity, http.StatusServiceUnavailable, ""},
	{prov_apps.ErrPortUnavailable, http.StatusServiceUnavailable, ""},
}

// knownFailure is err on the wire when zzRouter can name it; an unnamed error's status is the surface's call.
func knownFailure(err error) (httperr.Error, bool) {
	var absent cache.ErrModelNotFound
	if errors.As(err, &absent) {
		return httperr.Error{Status: http.StatusNotFound, Type: "invalid_request_error", Code: string(httperr.CodeUnknownModel), Message: absent.Error()}, true
	}
	var classified *httperr.EngineFailure
	if errors.As(err, &classified) {
		return (&httperr.EngineFailure{Class: classified.Class, Message: err.Error()}).HTTPError(), true
	}
	if wire, ok := renderAdmissionError(err); ok {
		return wire, true
	}
	for _, row := range failureTable {
		if errors.Is(err, row.sentinel) {
			return httperr.Error{Status: row.status, Type: openaicompat.StatusToErrorType(row.status), Code: row.code, Message: utils.SanitizeErrorMessage(err.Error())}, true
		}
	}
	return httperr.Error{}, false
}
