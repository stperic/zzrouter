// package server provides HTTP handlers for the zzrouter host server.
// Model-load failure classification — shared by the streaming and
// non-streaming halves of HandleLocalModel so both report a failure the
// same way.

package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
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
	// retryAfter, when positive, is advertised as Retry-After. Set
	// only where retrying is genuinely the right move — a model still
	// coming up, not one that cannot come up.
	retryAfter time.Duration
	// extra rides beside the error as top-level members, such as the
	// run and job a still-loading model can be followed through.
	extra map[string]any
	// cause is the error this was built from, kept so the chain survives
	// classification. Without it errors.Is against anything underneath
	// (context.DeadlineExceeded, a transport error) silently stops
	// working the moment a failure crosses this layer.
	cause error
}

func (e *loadFailure) Error() string { return e.message }
func (e *loadFailure) Unwrap() error { return e.cause }

// wire is the failure as every dialect renders it.
func (e *loadFailure) wire() httperr.Error {
	return httperr.Error{Status: e.status, Type: e.errType, Code: e.code, Message: e.message, RetryAfter: e.retryAfter, Extra: e.extra}
}

// forRun names the run and job a caller can follow instead of retrying blind.
func (e *loadFailure) forRun(run *instance.Instance) *loadFailure {
	e.extra = map[string]any{"run_id": run.ID, "job_id": run.StreamJobID}
	return e
}

// withCause keeps the original error reachable through errors.Is/As.
func (e *loadFailure) withCause(err error) *loadFailure {
	e.cause = err
	return e
}

// loadEngineFailed reports a startup failure without a more specific cause.
func loadEngineFailed(message string) *loadFailure {
	return &loadFailure{status: http.StatusBadGateway, errType: "api_error", code: "model_load_failed", message: message}
}

// inferenceCode is an inference caller's code for a failure with none of its own.
func inferenceCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusServiceUnavailable:
		return "provider_unavailable"
	default:
		return "model_load_failed"
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
		status:     http.StatusServiceUnavailable,
		errType:    "api_error",
		code:       "model_load_timeout",
		message:    message,
		retryAfter: modelWarmupRetryAfter,
	}
}

// modelWarmupRetryAfter is how long to tell a caller to wait before
// asking again for a model that is still coming up. Short enough that an
// agent's retry lands near the moment the instance goes ready, long
// enough not to invite a hot loop against a model that takes minutes.
const modelWarmupRetryAfter = 5 * time.Second

// loadInternal is for states that mean zzRouter lost track of its own
// instance — a genuine server fault, unlike the rest.
func loadInternal(message string) *loadFailure {
	return &loadFailure{status: http.StatusInternalServerError, errType: "server_error", code: "model_load_failed", message: message}
}

// classifyLoadFailure maps any load error onto the wire through the
// failure table. An error no row names did not come from a refusal, so it
// lands on the same 502 the engine failures use rather than being dropped.
func classifyLoadFailure(err error) *loadFailure {
	// A named cause outranks a generic failure wrapped around it.
	if wire, ok := knownFailure(err); ok {
		if wire.Code == "" {
			wire.Code = inferenceCode(wire.Status)
		}
		return (&loadFailure{status: wire.Status, errType: wire.Type, code: wire.Code, message: wire.Message, retryAfter: wire.RetryAfter}).withCause(err)
	}
	var lf *loadFailure
	if errors.As(err, &lf) {
		return lf
	}
	return loadEngineFailed(fmt.Sprintf("failed to load model: %v", err)).withCause(err)
}
