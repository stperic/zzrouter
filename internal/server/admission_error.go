package server

import (
	"errors"
	"net/http"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// renderAdmissionError is an instance's refusal as the wire answers it: 503 with Retry-After,
// since the server is busy, not the caller over quota.
func renderAdmissionError(err error) (httperr.Error, bool) {
	var admission *instance.AdmissionError
	if !errors.As(err, &admission) {
		return httperr.Error{}, false
	}
	code := httperr.CodeCapacityExceeded
	message := "This model's request queue is full. Please retry shortly."
	if errors.Is(admission, instance.ErrQueueWaitExpired) {
		code = httperr.CodeQueueWaitExpired
		message = "The wait for this model's request slot expired. Please retry shortly."
	}
	return httperr.Error{
		Status:     http.StatusServiceUnavailable,
		Type:       openaicompat.StatusToErrorType(http.StatusServiceUnavailable),
		Code:       string(code),
		Message:    message,
		RetryAfter: admission.RetryAfter,
	}, true
}
