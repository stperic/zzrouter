package fallback

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// ErrUnreachable marks a deployment this node has no way to reach (no
// client for its transport, say). Like a refused connection, it moves on
// to the next deployment rather than ending the chain.
var ErrUnreachable = errors.New("deployment unreachable from this node")

// IsRetriable returns true if the HTTP status code or error indicates a retriable failure.
//
// Retriable: 429 (rate limit), 402 (quota), 503 (unavailable), 504 (timeout), connection errors.
// Non-retriable: 400, 401, 403, 404, 422, and any other client error → return immediately.
func IsRetriable(statusCode int, err error) bool {
	// Transport-level errors are always retriable
	if err != nil {
		return isTransportError(err)
	}

	switch statusCode {
	case 429, 402, 503, 504:
		return true
	default:
		return false
	}
}

// isTransportError checks if the error is a connection-level failure
// (refused, timeout, DNS, etc.) that warrants trying the next deployment.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, ErrUnreachable) {
		return true
	}

	// Connection refused
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// DNS failures
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// Timeouts
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Connection reset / broken pipe
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	// EOF (connection closed unexpectedly)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	return false
}
