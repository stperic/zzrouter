// package server provides HTTP handlers for the zzrouter host server.
// Model-load failure classification — shared by the streaming and
// non-streaming halves of HandleLocalModel so both report a failure the
// same way.

package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// loadFailure is a model-load failure that already knows how it should
// reach the client. The executor distinguishes "model file was never
// downloaded" from "the engine died on startup", but the seam used to
// collapse every cause to a bare string and the caller mapped all of
// them to 500 server_error. A missing model then looked to an agent like
// a transient server fault, so it retried something that could never
// succeed.
type loadFailure struct {
	status  int    // HTTP status
	errType string // OpenAI error.type
	code    string // machine-readable error.code
	message string
	// retryAfterSecs, when non-zero, is advertised as Retry-After. Set
	// only where retrying is genuinely the right move — a model still
	// coming up, not one that cannot come up.
	retryAfterSecs int
	// cause is the error this was built from, kept so the chain survives
	// classification. Without it errors.Is against anything underneath
	// (context.DeadlineExceeded, a transport error) silently stops
	// working the moment a failure crosses this layer.
	cause error
}

func (e *loadFailure) Error() string { return e.message }
func (e *loadFailure) Unwrap() error { return e.cause }

// withCause keeps the original error reachable through errors.Is/As.
func (e *loadFailure) withCause(err error) *loadFailure {
	e.cause = err
	return e
}

// newLoadFailure builds a failure from the executor's own status code,
// which is the only place that knows why the load did not happen.
func newLoadFailure(executorStatus int, message string) *loadFailure {
	switch executorStatus {
	case http.StatusBadRequest:
		return &loadFailure{status: http.StatusBadRequest, errType: "invalid_request_error", code: "invalid_request", message: message}
	case http.StatusNotFound:
		// Same envelope as HandleLocalModel's no-capable-provider case:
		// from the caller's side both mean "this model cannot serve you".
		return &loadFailure{status: http.StatusNotFound, errType: "invalid_request_error", code: "model_not_found", message: message}
	case http.StatusServiceUnavailable:
		return &loadFailure{status: http.StatusServiceUnavailable, errType: "api_error", code: "provider_unavailable", message: message}
	default:
		// The engine was reachable and failed anyway — a gateway
		// reporting a bad upstream, matching this package's other
		// backend failures.
		return &loadFailure{status: http.StatusBadGateway, errType: "api_error", code: "model_load_failed", message: message}
	}
}

// loadStillWarming is a cold start that has not finished inside the
// request's budget. Nothing says the model is broken — the load is still
// running and a later request will very likely be served, which is
// exactly what 503 + Retry-After means. A 504 would read as "the
// upstream gave up", and a client that distinguishes them would stop
// retrying the one case where retrying works.
func loadStillWarming(message string) *loadFailure {
	return &loadFailure{
		status:         http.StatusServiceUnavailable,
		errType:        "api_error",
		code:           "model_load_timeout",
		message:        message,
		retryAfterSecs: modelWarmupRetryAfterSecs,
	}
}

// modelWarmupRetryAfterSecs is how long to tell a caller to wait before
// asking again for a model that is still coming up. Short enough that an
// agent's retry lands near the moment the instance goes ready, long
// enough not to invite a hot loop against a model that takes minutes.
const modelWarmupRetryAfterSecs = 5

// loadInternal is for states that mean zzRouter lost track of its own
// instance — a genuine server fault, unlike the rest.
func loadInternal(message string) *loadFailure {
	return &loadFailure{status: http.StatusInternalServerError, errType: "server_error", code: "model_load_failed", message: message}
}

// classifyLoadFailure maps any load error onto the wire. An error that
// did not come from the load path at all still has to reach the client,
// so it lands on the same 502 the engine failures use rather than being
// dropped.
func classifyLoadFailure(err error) *loadFailure {
	var memory *process.MemoryFitError
	if errors.As(err, &memory) {
		return (&loadFailure{status: http.StatusConflict, errType: "invalid_request_error", code: string(httperr.CodeOutOfRange), message: memory.Error()}).withCause(err)
	}
	if errors.Is(err, process.ErrMemoryBudgetInvalid) {
		return newLoadFailure(http.StatusBadRequest, err.Error()).withCause(err)
	}
	if errors.Is(err, process.ErrMemoryObservation) || errors.Is(err, prov_apps.ErrAtCapacity) {
		return newLoadFailure(http.StatusServiceUnavailable, err.Error()).withCause(err)
	}
	if errors.Is(err, config.ErrModelNameConflict) {
		return (&loadFailure{status: http.StatusConflict, errType: "invalid_request_error", code: "model_name_conflict", message: err.Error()}).withCause(err)
	}
	var lf *loadFailure
	if errors.As(err, &lf) {
		return lf
	}
	return newLoadFailure(http.StatusBadGateway, fmt.Sprintf("failed to load model: %v", err)).withCause(err)
}
