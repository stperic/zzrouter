package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

// failureRow is one decided answer; an empty code leaves each surface its generic one.
type failureRow struct {
	sentinel                      error
	status                        int
	code, message, title, errType string
	retryAfter                    time.Duration
	// resolve matches a typed payload and fills only its dynamic wire facts.
	resolve func(error, *httperr.Error) bool
	extra   func(error, httperr.Error) map[string]any
}

// failureTable is the decision record every surface reads; error_status_guard_test.go holds it complete.
var failureTable = []failureRow{
	{resolve: resolveExplicitWire},
	{status: http.StatusNotFound, code: string(httperr.CodeUnknownModel), errType: "invalid_request_error", resolve: resolveCatalogMiss},
	{title: "GPU memory budget cannot fit", resolve: resolveMemoryFit, extra: memoryFailureExtra},
	{resolve: resolveEngineFailure},
	{sentinel: instance.ErrQueueWaitExpired, status: http.StatusServiceUnavailable, code: string(httperr.CodeQueueWaitExpired), message: "The wait for this model's request slot expired. Please retry shortly.", resolve: resolveAdmission},
	{sentinel: instance.ErrStopping, status: http.StatusServiceUnavailable, code: string(httperr.CodeCapacityExceeded), message: "This model's run is stopping. Please retry shortly.", resolve: resolveAdmission},
	{status: http.StatusServiceUnavailable, code: string(httperr.CodeCapacityExceeded), message: "This model's request queue is full. Please retry shortly.", resolve: resolveAdmission},

	{sentinel: prov_apps.ErrFeatureEvidence, status: http.StatusConflict, code: "model_feature_evidence_invalid"},
	{sentinel: config.ErrModelNameConflict, status: http.StatusConflict, code: "model_name_conflict"},
	// Half-downloaded weights: the caller must repair the model, which 409 says and 400 does not.
	{sentinel: prov_apps.ErrModelIncomplete, status: http.StatusConflict, code: "model_incomplete"},

	{sentinel: errAutoDetectModelNotFound, status: http.StatusNotFound, code: ""},
	{sentinel: errAutoDetectMultipleProviders, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrProviderNotFound, status: http.StatusNotFound, code: ""},
	{sentinel: prov_apps.ErrInstanceNotFound, status: http.StatusNotFound, code: ""},

	{sentinel: prov_apps.ErrExternalService, status: http.StatusForbidden, code: ""},

	{sentinel: prov_apps.ErrInstallInProgress, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrServiceDisabled, status: http.StatusConflict, code: ""},
	{sentinel: install.ErrStalePlan, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrRunHeldByOtherProvider, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrInstancesRunning, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrPinRunActive, status: http.StatusConflict, code: "pin_run_active"},
	{sentinel: prov_apps.ErrProviderNotManaged, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrProviderAlreadyInstalled, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrInstanceAlreadyExists, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrModelAlreadyLoaded, status: http.StatusConflict, code: ""},
	{sentinel: prov_apps.ErrPortConflict, status: http.StatusConflict, code: ""},

	{sentinel: prov_apps.ErrUnsupportedPlatform, status: http.StatusBadRequest, code: ""},
	{sentinel: process.ErrMemoryBudgetInvalid, status: http.StatusBadRequest, code: ""},
	{sentinel: prov_apps.ErrModelNotLocal, status: http.StatusNotFound, code: "", resolve: resolveModelNotLocal},
	{sentinel: prov_apps.ErrParameterValidation, status: http.StatusBadRequest, resolve: resolveParameterIssues, extra: parameterIssuesExtra},
	{sentinel: prov_apps.ErrParameterValidation, status: http.StatusBadRequest, code: ""},
	{sentinel: prov_apps.ErrPortOutOfRange, status: http.StatusBadRequest, code: ""},
	{sentinel: prov_apps.ErrDangerousEnvVar, status: http.StatusBadRequest, code: ""},

	{sentinel: prov_apps.ErrProviderUnavailable, status: http.StatusServiceUnavailable, code: "provider_unavailable"},
	// The daemon dropped the model: the next request loads it again.
	{sentinel: prov_apps.ErrModelDropped, status: http.StatusServiceUnavailable, code: ""},
	{sentinel: prov_apps.ErrShutdown, status: http.StatusServiceUnavailable, code: ""},
	{sentinel: process.ErrMemoryObservation, status: http.StatusServiceUnavailable, code: ""},
	{sentinel: prov_apps.ErrAtCapacity, status: http.StatusServiceUnavailable, code: ""},
	{sentinel: prov_apps.ErrPortUnavailable, status: http.StatusServiceUnavailable, code: ""},
	{sentinel: errLoadWarming, status: http.StatusServiceUnavailable, code: "model_load_timeout", retryAfter: modelWarmupRetryAfter, resolve: resolveLoadState, extra: warmingFailureExtra},
	{sentinel: errLoadInternal, status: http.StatusInternalServerError, code: "model_load_failed", errType: "server_error", resolve: resolveLoadState},
	{sentinel: errLoadFailed, status: http.StatusBadGateway, code: "model_load_failed", resolve: resolveLoadState},
	{status: http.StatusBadRequest, code: "", resolve: resolveInvalidInput},
}

// knownFailure is the one table lookup for every known run refusal.
func knownFailure(err error) (httperr.Error, bool) {
	if err == nil {
		return httperr.Error{}, false
	}
	for _, row := range failureTable {
		if row.sentinel != nil && !errors.Is(err, row.sentinel) {
			continue
		}
		wire := httperr.Error{Status: row.status, Type: openaicompat.StatusToErrorType(row.status), Code: row.code, Message: utils.SanitizeErrorMessage(err.Error()), RetryAfter: row.retryAfter}
		if row.resolve != nil && !row.resolve(err, &wire) {
			continue
		}
		if row.message != "" {
			wire.Message = row.message
		}
		if row.title != "" {
			wire.Title = row.title
		}
		if row.errType != "" {
			wire.Type = row.errType
		}
		if row.extra != nil {
			wire.Extra = row.extra(err, wire)
		}
		return wire, true
	}
	return httperr.Error{}, false
}

func resolveExplicitWire(err error, wire *httperr.Error) bool {
	return errors.As(err, wire)
}

func resolveInvalidInput(err error, _ *httperr.Error) bool {
	return isInvalidInput(err)
}

func resolveCatalogMiss(err error, wire *httperr.Error) bool {
	var absent cache.ErrModelNotFound
	if !errors.As(err, &absent) {
		return false
	}
	wire.Message = absent.Error()
	return true
}

func resolveEngineFailure(err error, wire *httperr.Error) bool {
	var classified *httperr.EngineFailure
	if !errors.As(err, &classified) {
		return false
	}
	*wire = (&httperr.EngineFailure{Class: classified.Class, Message: err.Error()}).HTTPError()
	return true
}

func resolveMemoryFit(err error, wire *httperr.Error) bool {
	var fit *process.MemoryFitError
	return errors.As(err, &fit) && resolveEngineFailure(err, wire)
}

func memoryFailureExtra(err error, wire httperr.Error) map[string]any {
	var memory *process.MemoryFitError
	if !errors.As(err, &memory) {
		return nil
	}
	hint := "Inspect holders with zz_runs and zz_logs, accepted settings with zz_params_get, then use zz_run_stop or zz_params_set as appropriate and zz_run_preview before zz_run_start."
	if memory.Sized || memory.KeptRun != "" && memory.BudgetKind != "" {
		hint = "Inspect accepted settings with zz_params_get and candidates with zz_models and zz_nodes; use zz_run_preview with a supported budget or numeric context, then zz_run_start."
	}
	if memory.KeptRun != "" && memory.BudgetKind == "" {
		hint = "Lower the request with zz_params_set and zz_run_preview, or stop other holders with zz_run_stop; stop kept_run first only when holders include other_or_untracked memory zzRouter could not attribute, then zz_run_start. When holders show unused_commitment_mib, only stopping those holders with zz_run_stop helps."
	}
	return map[string]any{"errors": []utils.ParamError{{Key: memory.Parameter, Code: wire.Code, Got: memory, Max: memory.FreeMiB, Want: "required_mib <= free_mib on each selected device", Message: wire.Message, Hint: hint}}}
}

// resolveParameterIssues repeats the per-key code when every refused parameter shares it.
func resolveParameterIssues(err error, wire *httperr.Error) bool {
	var refused *prov_apps.ParameterError
	if !errors.As(err, &refused) {
		return false
	}
	wire.Code = sharedParamCode(refused.Issues)
	return true
}

func parameterIssuesExtra(err error, _ httperr.Error) map[string]any {
	var refused *prov_apps.ParameterError
	if !errors.As(err, &refused) {
		return nil
	}
	return map[string]any{"errors": refused.ParamIssues()}
}

func resolveAdmission(err error, wire *httperr.Error) bool {
	var admission *instance.AdmissionError
	if !errors.As(err, &admission) {
		return false
	}
	wire.RetryAfter = admission.RetryAfter
	return true
}

func resolveLoadState(err error, wire *httperr.Error) bool {
	var state *loadState
	if !errors.As(err, &state) {
		return false
	}
	wire.Message = utils.SanitizeErrorMessage(state.message)
	if state.runID != "" && state.jobID == "" {
		wire.Message += "; job outcome unavailable; inspect zz_runs"
	}
	return true
}

func warmingFailureExtra(err error, _ httperr.Error) map[string]any {
	var state *loadState
	if !errors.As(err, &state) || state.runID == "" {
		return nil
	}
	if state.jobID == "" {
		return map[string]any{"run_id": state.runID}
	}
	return map[string]any{"run_id": state.runID, "job_id": state.jobID}
}

func resolveModelNotLocal(_ error, wire *httperr.Error) bool {
	wire.Message += "; download or register it before starting; use POST /zzrouter/v1/deployments for a cloud model"
	return true
}
