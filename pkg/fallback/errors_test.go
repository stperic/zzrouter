package fallback

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReplaySafeClassification(t *testing.T) {
	type testCase struct {
		name, method  string
		err           error
		status        int
		written, want bool
	}
	tests := []testCase{
		{"dial refused", "POST", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, 0, false, true},
		{"DNS", "POST", &net.DNSError{Err: "no such host", Name: "missing.test"}, 0, false, true},
		{"TLS handshake", "POST", tls.RecordHeaderError{Msg: "invalid TLS header"}, 0, false, true},
		{"unreachable", "POST", fmt.Errorf("wrapped: %w", ErrUnreachable), 0, true, true},
		{"reset written", "POST", syscall.ECONNRESET, 0, true, false},
		{"read OpError written", "POST", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, 0, true, false},
		{"pipe written", "POST", syscall.EPIPE, 0, true, false},
		{"write OpError written", "POST", &net.OpError{Op: "write", Err: syscall.EPIPE}, 0, true, false},
		{"timeout written", "POST", os.ErrDeadlineExceeded, 0, true, false},
		{"429", "POST", nil, 429, true, true},
		{"402", "POST", nil, 402, true, true},
		{"503", "POST", nil, 503, true, true},
		{"504", "POST", nil, 504, true, false},
		{"GET reset", "GET", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, 0, true, true},
		{"GET timeout", "GET", os.ErrDeadlineExceeded, 0, true, true},
		{"any error unwritten", "POST", errors.New("request preparation failed"), 0, false, true},
		{"error precedes status", "POST", syscall.ECONNRESET, 429, true, false},
		{"unknown written", "POST", errors.New("unknown failure"), 0, true, false},
	}
	for _, status := range []int{200, 400, 401, 403, 404, 422, 500, 502} {
		tests = append(tests, testCase{fmt.Sprint(status), "POST", nil, status, true, false})
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete} {
		tests = append(tests, testCase{method + " 504", method, nil, 504, true, true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ReplaySafe(tt.method, tt.err, tt.status, tt.written))
		})
	}
}
