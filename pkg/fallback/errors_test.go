package fallback

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsRetriable_HTTPStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"429 rate limit", 429, true},
		{"402 quota", 402, true},
		{"503 unavailable", 503, true},
		{"504 timeout", 504, true},
		{"400 bad request", 400, false},
		{"401 unauthorized", 401, false},
		{"403 forbidden", 403, false},
		{"404 not found", 404, false},
		{"422 unprocessable", 422, false},
		{"200 success", 200, false},
		{"500 internal error", 500, false},
		{"502 bad gateway", 502, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRetriable(tt.statusCode, nil)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsRetriable_TransportErrors(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		assert.True(t, IsRetriable(0, err))
	})

	t.Run("DNS error", func(t *testing.T) {
		err := &net.DNSError{Err: "no such host", Name: "example.com"}
		assert.True(t, IsRetriable(0, err))
	})

	t.Run("connection reset", func(t *testing.T) {
		assert.True(t, IsRetriable(0, syscall.ECONNRESET))
	})

	t.Run("broken pipe", func(t *testing.T) {
		assert.True(t, IsRetriable(0, syscall.EPIPE))
	})

	t.Run("nil error", func(t *testing.T) {
		assert.False(t, IsRetriable(200, nil))
	})

	t.Run("generic error is not retriable", func(t *testing.T) {
		assert.False(t, IsRetriable(0, errors.New("something else")))
	})
}

func TestIsRetriable_ErrorTakesPrecedence(t *testing.T) {
	// When there's a transport error, it's retriable regardless of status code
	err := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	assert.True(t, IsRetriable(400, err))
}
