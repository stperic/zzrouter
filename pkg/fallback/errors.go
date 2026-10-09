package fallback

import (
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
)

// ErrUnreachable marks a deployment this node has no way to reach.
var ErrUnreachable = errors.New("deployment unreachable from this node")

// RequestError preserves whether a failed send could have reached the upstream.
type RequestError struct {
	Err          error
	WroteRequest bool
}

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// ReplaySafe decides whether another send can repeat the operation safely.
func ReplaySafe(method string, err error, status int, wroteRequest bool) bool {
	idempotent := false
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		idempotent = true
	}
	if err == nil {
		switch status {
		case http.StatusTooManyRequests, http.StatusPaymentRequired, http.StatusServiceUnavailable:
			return true
		case http.StatusGatewayTimeout:
			return idempotent
		default:
			return false
		}
	}
	if !wroteRequest || errors.Is(err, ErrUnreachable) {
		return true
	}
	if !idempotent {
		return false
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var netErr net.Error
	return errors.As(err, &opErr) || errors.As(err, &dnsErr) ||
		(errors.As(err, &netErr) && netErr.Timeout()) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}
