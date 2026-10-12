package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

var (
	errLoadFailed   = errors.New("model load failed")
	errLoadWarming  = errors.New("model load still warming")
	errLoadInternal = errors.New("model load state lost")
)

// loadState carries readiness facts and a cause, never an HTTP decision.
type loadState struct {
	cause        error
	message      string
	runID, jobID string
}

func (e *loadState) Error() string { return e.message }
func (e *loadState) Unwrap() error { return e.cause }
func (e *loadState) forRun(run *instance.Instance) *loadState {
	e.runID, e.jobID = run.ID, run.StreamJobID
	return e
}
func (e *loadState) withCause(err error) *loadState { e.cause = errors.Join(e.cause, err); return e }
func loadEngineFailed(message string) *loadState {
	return &loadState{cause: errLoadFailed, message: message}
}
func loadStillWarming(message string) *loadState {
	return &loadState{cause: errLoadWarming, message: message}
}
func loadInternal(message string) *loadState {
	return &loadState{cause: errLoadInternal, message: message}
}

// modelWarmupRetryAfter is the delay suggested while an admitted model still starts.
const modelWarmupRetryAfter = 5 * time.Second

// loadFailureWire applies only the inference surface's unnamed fallback and default code.
func loadFailureWire(err error) httperr.Error {
	wire := failureWire(err, http.StatusBadGateway)
	code := inferenceCode(err, wire)
	if wire.Code == "" && code != "" && wire.Status == http.StatusBadGateway {
		wire.Message = "failed to load model: " + wire.Message
	}
	wire.Code = code

	return wire
}

// inferenceCode supplies the inference vocabulary only when the table assigned no code.
func inferenceCode(err error, wire httperr.Error) string {
	var explicit httperr.Error
	if wire.Code != "" || errors.As(err, &explicit) {
		return wire.Code
	}
	switch wire.Status {
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
