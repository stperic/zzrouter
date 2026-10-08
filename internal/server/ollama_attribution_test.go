package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ndjsonWorker stands up a fake worker that answers the way Ollama does:
// newline-delimited JSON under application/x-ndjson, every frame naming the
// model the engine knows.
func ndjsonWorker(t *testing.T, engineModel string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`{"model":"` + engineModel + `","message":{"role":"assistant","content":"hi"},"done":false}`,
			`{"model":"` + engineModel + `","message":{"role":"assistant","content":""},"done":true}`,
		} {
			_, _ = w.Write([]byte(frame + "\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A cluster-routed /api/chat stream must answer in the caller's own spelling.
//
// proxyToWorkerURL decided "is this a stream?" with a hand-rolled check that
// knew text/event-stream and chunked but not application/x-ndjson — which is
// exactly how Ollama streams. Every /api/chat stream therefore took the
// non-streaming branch, the per-chunk transforms never ran, and each frame
// came back as the bare engine model. A client streaming from the Ollama
// surface had no signal at all about which node served it.
func TestProxyToWorkerURL_StreamKeepsCallerModelSpelling(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	baseURL := ndjsonWorker(t, "smollm:135m")

	body := []byte(`{"model":"smollm:135m","messages":[],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// What stripNodeHint stashes when the caller wrote "model@node".
	req = req.WithContext(context.WithValue(req.Context(), CtxKeyClientModel, "smollm:135m@worker-1"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	s.proxyToWorkerURL(c, baseURL, "worker-1", "ollama", body, http.DefaultClient)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	frames := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var probe map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &probe), "frame must stay parseable: %q", line)
		assert.Equal(t, "smollm:135m@worker-1", probe["model"],
			"every frame carries the model, so every frame must name the node")
		frames++
	}
	assert.Equal(t, 2, frames, "both frames must survive the relay")
}

// The same relay must not rewrite anything when the caller's spelling already
// is the engine's. Guards against a restore that fires unconditionally and
// invents an @node suffix for a caller who never asked for one.
func TestProxyToWorkerURL_StreamLeavesUndecoratedModelAlone(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	baseURL := ndjsonWorker(t, "smollm:135m")

	body := []byte(`{"model":"smollm:135m","messages":[],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	s.proxyToWorkerURL(c, baseURL, "worker-1", "ollama", body, http.DefaultClient)

	require.Equal(t, http.StatusOK, w.Code)
	frames := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var probe map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &probe))
		assert.Equal(t, "smollm:135m", probe["model"])
		frames++
	}
	// Without this the test passes against the pre-fix code too: with no
	// ClientModel to restore, the non-streaming branch also leaves the model
	// alone, so the assertion above cannot tell the two branches apart. Two
	// frames arriving separately is what says the relay streamed.
	assert.Equal(t, 2, frames, "both frames must arrive, which only the streaming branch does")
}

// Nothing on the Ollama surface ever called SetStream, so every /api/*
// request was logged stream:false — including the streaming ones. Ollama's
// default is the inverse of OpenAI's, which is the part that makes an absent
// field load-bearing rather than a don't-care.
func TestOllamaStreamRequested(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		endpointType string
		want         bool
	}{
		{"explicit true", `{"model":"m","stream":true}`, "chat", true},
		{"explicit false", `{"model":"m","stream":false}`, "chat", false},
		{"absent defaults to true on chat", `{"model":"m"}`, "chat", true},
		{"absent defaults to true on generate", `{"model":"m"}`, "generate", true},
		{"embeddings never stream", `{"model":"m"}`, "embeddings", false},
		{"embed never streams even if asked", `{"model":"m","stream":true}`, "embed", false},
		// A body we cannot read must not be reported as non-streaming: the
		// surface's default is to stream, and guessing the rarer answer
		// would undercount exactly the requests this flag exists to find.
		{"unparseable body falls back to the surface default", `{not json`, "chat", true},
		{"empty body falls back to the surface default", ``, "chat", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ollamaStreamRequested([]byte(tt.body), tt.endpointType))
		})
	}
}
