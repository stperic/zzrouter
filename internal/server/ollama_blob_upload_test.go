package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
)

// A blob reaches the daemon with its length, not chunked, and without the
// caller's key: the daemon is sent what its provider config declares.
func TestCreateBlob_ForwardsLengthAndNoKey(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 4096)
	var got *http.Request
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(daemon.Close)
	svc := &OllamaServiceImpl{
		ollamaDaemon: func() (*backend.Resolved, error) {
			return &backend.Resolved{Endpoint: daemon.URL, Upstream: backend.Engine()}, nil
		},
		httpStreamingClient: daemon.Client(),
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/blobs/sha256:abc", bytes.NewReader(blob))
	c.Request.Header.Set("X-API-Key", TestAdminKey)

	require.NoError(t, svc.CreateBlob(c, "sha256:abc"))
	require.NotNil(t, got, "the daemon was not reached")
	assert.Equal(t, int64(len(blob)), got.ContentLength)
	assert.Empty(t, got.TransferEncoding)
	assert.Empty(t, got.Header.Get("X-API-Key"))
	assert.Equal(t, http.StatusCreated, c.Writer.Status(), "the daemon's answer is relayed")
}

// The calls zzRouter makes to the Ollama daemon on its own carry the
// credential the provider declares, as the forwards do.
func TestOllamaDirectCallsCarryTheDaemonsCredential(t *testing.T) {
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	daemon := func() (*backend.Resolved, error) {
		return &backend.Resolved{Endpoint: srv.URL, Upstream: backend.Credential(
			&config.APIConfig{AuthType: config.AuthTypeBearer, Token: "daemon-token"})}, nil
	}
	svc := &OllamaServiceImpl{ollamaDaemon: daemon, httpClient: srv.Client()}
	exec := NewInternalExecutor(nil, nil, daemon, srv.Client(), nil, nil, nil, nil, nil)

	ctx := context.Background()
	d, _ := daemon()
	calls := map[string]func(){
		"HEAD /api/blobs/sha256:abc":  func() { _, _ = svc.CheckBlob(ctx, "sha256:abc") },
		"POST /api/show (pre-flight)": func() { _, _ = svc.ollamaModelExists(ctx, d, "m") },
		"POST /api/show (details)":    func() { _, _ = exec.getOllamaModelDetails(ctx, "m", false) },
	}
	for name, call := range calls {
		clear(seen)
		call()
		require.Len(t, seen, 1, name)
		for _, auth := range seen {
			assert.Equal(t, "Bearer daemon-token", auth, name)
		}
	}
}
