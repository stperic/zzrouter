package retry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
)

// Decision is the result of classifying an error against a retry policy.
type Decision int

const (
	// Retry — transient error, call the operation again after backoff.
	Retry Decision = iota
	// Stop — terminal error, surface it up.
	Stop
)

// Classifier inspects an error and reports whether the retry loop
// should continue. One Classifier per loop site keeps per-call-site
// tuning possible without bleeding policy into the errors themselves.
type Classifier func(err error) Decision

// ClassifyNetwork is the default classifier for network-flavored retry
// loops (dispatch, pairing poll, health probe). Stops on:
//   - context cancellation or deadline
//   - TLS certificate verification failures
//   - DNS not-found errors (NXDOMAIN)
//   - any of the caller-supplied permanentSentinels (via errors.Is)
//
// Retries all other errors.
func ClassifyNetwork(permanentSentinels ...error) Classifier {
	return func(err error) Decision {
		if err == nil {
			return Stop
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Stop
		}
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return Stop
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return Stop
		}
		for _, s := range permanentSentinels {
			if errors.Is(err, s) {
				return Stop
			}
		}
		return Retry
	}
}
