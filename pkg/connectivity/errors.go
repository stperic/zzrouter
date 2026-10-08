package connectivity

import (
	"errors"
	"net"
	"net/http"
)

// ErrPermanent marks errors that retrying cannot resolve. Wrap upstream errors
// with this sentinel (via fmt.Errorf("...: %w", ErrPermanent)) at the boundary
// where the permanent condition is recognized — e.g. an HTTP 401 response or a
// malformed request URL — and check with errors.Is downstream.
var ErrPermanent = errors.New("permanent provider error")

// permanentHTTPStatus reports whether an HTTP status code indicates an
// unrecoverable condition: auth failure, missing endpoint, or wrong method.
// 5xx and most 4xx codes are excluded — those can succeed on retry once the
// upstream recovers or the request races a config reload.
func permanentHTTPStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed:
		return true
	}
	return false
}

// isPermanentNetError reports whether a network-level error is unrecoverable.
// Currently: DNS name resolution returning NotFound.
func isPermanentNetError(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return true
	}
	return false
}
