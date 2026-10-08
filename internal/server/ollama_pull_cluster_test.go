package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pins Gap #1 from the /api/* unification arc: cross-cluster /api/pull
// must (1) relay the worker's terminal-success Ollama frame to the
// client (pre-fix the body was empty because Transfer-Encoding:chunked
// was forwarded over an already-de-chunked body) and (2) invalidate
// coord's catalog so the worker-pulled model shows up on /v1/models
// without waiting for the periodic peer-sync.

// chunkedSuccessRemote returns a stub worker that replies with a
// chunked NDJSON success frame — the same encoding the worker's gin
// writer produces when streamResponseWithFlush relays a non-streaming
// Ollama daemon response. This is the shape that pre-fix coord ate.
func chunkedSuccessRemote(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/pull", r.URL.Path,
			"cluster route must POST to worker /api/pull (the canonical Ollama-native name)")
		w.Header().Set("Content-Type", "application/x-ndjson")
		// httptest.Server's ResponseWriter implements http.Flusher, so
		// writing without setting Content-Length triggers chunked
		// encoding — exactly what the worker's gin writer does.
		w.WriteHeader(status)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestPullClusterRoute_RelaysSuccessFrame(t *testing.T) {
	remote := chunkedSuccessRemote(t, http.StatusOK, `{"status":"success"}`+"\n")

	var invalidates atomic.Int32
	svc := &OllamaServiceImpl{
		hasRouter:           true,
		httpStreamingClient: http.DefaultClient,
		getClusterNodeMTLSURL: func(node string) (string, error) {
			require.Equal(t, "worker-1", node)
			return remote.URL, nil
		},
		getMTLSClient:        func() *http.Client { return http.DefaultClient },
		invalidateModelCache: func() { invalidates.Add(1) },
	}

	body := []byte(`{"model":"smollm:135m@worker-1","stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "/api/pull", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	stream := false
	err := svc.pullClusterRoute(c, "worker-1", "smollm:135m", false, &stream)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, w.Code,
		"upstream 200 must propagate, got %d body=%q", w.Code, w.Body.String())

	// Body must contain the success frame — pre-fix it was "".
	assert.Contains(t, w.Body.String(), `"status":"success"`,
		"client must see the worker's terminal success frame, got %q", w.Body.String())
	var frame map[string]string
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &frame))
	assert.Equal(t, "success", frame["status"])

	// 2xx must trigger the coord-side cache invalidate so /v1/models
	// surfaces the worker-pulled model without waiting on peer-sync.
	assert.Equal(t, int32(1), invalidates.Load(),
		"cluster pull must invalidate coord's model cache on success")

	// Hop-by-hop framing headers from upstream must NOT be re-published
	// — letting them through is what caused the empty-body symptom
	// (chunked label over an already-de-chunked body). The strip goes
	// through wire.StripHopByHop, so we check a representative subset
	// of the RFC 7230 §6.1 set to pin the contract.
	for _, h := range []string{
		"Transfer-Encoding", "Content-Length", "Content-Encoding",
		"Connection", "Keep-Alive", "Trailer",
	} {
		assert.Empty(t, w.Header().Get(h),
			"hop-by-hop header %q must be stripped before relay", h)
	}
}

func TestPullClusterRoute_NoInvalidateOn4xx(t *testing.T) {
	// Worker rejects the pull (e.g. registry unavailable).
	remote := chunkedSuccessRemote(t, http.StatusNotFound, `{"error":"manifest not found"}`+"\n")

	var invalidates atomic.Int32
	svc := &OllamaServiceImpl{
		hasRouter:             true,
		httpStreamingClient:   http.DefaultClient,
		getClusterNodeMTLSURL: func(string) (string, error) { return remote.URL, nil },
		getMTLSClient:         func() *http.Client { return http.DefaultClient },
		invalidateModelCache:  func() { invalidates.Add(1) },
	}

	body := []byte(`{"model":"nope:1b@worker-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/pull", bytes.NewReader(body))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	err := svc.pullClusterRoute(c, "worker-1", "nope:1b", false, nil)
	require.NoError(t, err)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "manifest not found",
		"4xx body must still be relayed to the client")
	assert.Equal(t, int32(0), invalidates.Load(),
		"non-2xx must NOT invalidate the cache — leaving it intact preserves the pre-pull catalog")
}

func TestPullClusterRoute_NoRouter_503(t *testing.T) {
	svc := &OllamaServiceImpl{hasRouter: false}

	req := httptest.NewRequest(http.MethodPost, "/api/pull", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	err := svc.pullClusterRoute(c, "worker-1", "x", false, nil)
	require.Error(t, err, "no router → must surface ServiceUnavailable problem")
}
