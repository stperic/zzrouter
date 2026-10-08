package server

import (
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stretchr/testify/assert"
)

// TestValidateSyncSourceURL asserts /sync/deploy source URLs are
// accepted only when backed by a known cluster member. With no
// registry (standalone/test), only loopback passes.
func TestValidateSyncSourceURL(t *testing.T) {
	// Build an executor with a no-endpoints cluster view so we exercise
	// the loopback-only fallback. The cluster-registered path is covered
	// by the end-to-end sync tests.
	e := NewSyncExecutor(
		func() *http.Client { return nil },
		9091,
		nil,
		nil,
		func() []*mesh.Endpoint { return nil },
	)

	cases := []struct {
		name    string
		url     string
		wantErr string // empty = should pass
	}{
		{"loopback IPv4 allowed", "http://127.0.0.1:9090", ""},
		{"loopback IPv6 allowed", "http://[::1]:9090", ""},
		{"localhost allowed", "http://localhost:9090", ""},
		{"invalid scheme", "ftp://127.0.0.1/x", "scheme"},
		{"missing host", "http:///x", "host"},
		{"unspecified 0.0.0.0", "http://0.0.0.0/", "unspecified"},
		{"link-local IPv4", "http://169.254.169.254/", "link-local"},
		{"non-loopback RFC1918 in standalone", "http://10.0.0.5/", "only loopback"},
		{"non-loopback public in standalone", "http://example.com/", "only loopback"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.validateSyncSourceURL(tc.url)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}
