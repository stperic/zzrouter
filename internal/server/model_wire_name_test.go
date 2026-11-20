package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
)

func withInferenceBody(req *http.Request, body []byte) *http.Request {
	c := gin.Context{Request: req}
	replaceRequestBody(&c, body)
	return c.Request
}

// instanceFor builds an instance pointed at a stub engine, speaking the
// given wire token.
func instanceFor(t *testing.T, engine *httptest.Server, model, wireModel string) *instance.Instance {
	t.Helper()
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	inst := instance.NewInstance("test-instance", "mlx", model, port, 0, 0)
	inst.WireModel = wireModel
	inst.MarkRunning(false, nil)
	return inst
}

func requestWithBody(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	ctx := context.WithValue(req.Context(), CtxKeyModel, "org/model")
	return withInferenceBody(req.WithContext(ctx), []byte(body))
}

func TestInstanceRequestUsesTranslatedPathAndAnswersAsClientModel(t *testing.T) {
	s, run := instanceStreamTestServer()
	t.Cleanup(run.Cancel)
	run.MarkRunning(false, nil)
	run.WireModel = "engine-id"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"model":"model","messages":[],"stream":true,"keep_alive":"30s"}`)
	ctx := context.WithValue(t.Context(), CtxKeyClientModel, "model@local")
	ctx = context.WithValue(ctx, CtxKeyRequestHints, keepalive.Parse(body))
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages?verbose=0", nil)
	restore := asChatRequest(c, body)
	defer restore()
	body = requestUsageCounts(c, body)
	req := newInstanceRequest(c.Request, body)
	prepared, answerAs := s.instanceRequest(req, run)
	assert.Equal(t, "model@local", answerAs)
	assert.JSONEq(t, `{"model":"engine-id","messages":[],"stream":true,"keep_alive":"30s","stream_options":{"include_usage":true}}`, string(prepared))
	lease, target, _, ok := s.openRun(req, run, nil, func(e httperr.Error) { t.Errorf("open run: %v", e) })
	require.True(t, ok)
	defer lease.Release()
	assert.Equal(t, chatCompletionsPath+"?verbose=0", target.RequestURI())
	assert.Equal(t, 30*time.Second, run.GetKeepAlive())
}

// TestProxyToInstance_TranslatesModelForPathEngines locks the contract for
// engines that load whatever token the request carries: they are addressed
// by weights path, and the client never sees that path back.
func TestProxyToInstance_TranslatesModelForPathEngines(t *testing.T) {
	const weightsPath = "/models/org/model"

	t.Run("non-streaming", func(t *testing.T) {
		var seen string
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.Unmarshal(body, &req))
			seen = req.Model

			// mlx_lm echoes whatever token it was asked for.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"` + req.Model + `","choices":[{"message":{"content":"hi"}}]}`))
		}))
		t.Cleanup(engine.Close)

		s := createTestNodeWithDefaults(t)
		rec := httptest.NewRecorder()
		const body = `{"model":"org/model","messages":[]}`
		req := requestWithBody(t, body)
		s.proxyToInstance(rec, req, newInstanceRequest(req, []byte(body)),
			instanceFor(t, engine, "org/model", weightsPath), true)

		assert.Equal(t, weightsPath, seen, "engine must be addressed by the token it keys on")

		var resp struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "org/model", resp.Model, "the client's name must come back, never the weights path")
	})

	t.Run("streaming", func(t *testing.T) {
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.Unmarshal(body, &req))

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"model\":\"" + req.Model + "\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		t.Cleanup(engine.Close)

		s := createTestNodeWithDefaults(t)
		rec := httptest.NewRecorder()
		const body = `{"model":"org/model","stream":true,"messages":[]}`
		req := requestWithBody(t, body)
		s.proxyToInstance(rec, req, newInstanceRequest(req, []byte(body)),
			instanceFor(t, engine, "org/model", weightsPath), true)

		out := rec.Body.String()
		assert.NotContains(t, out, weightsPath, "the weights path must not leak into the stream")
		assert.Contains(t, out, `"model":"org/model"`)
		assert.Contains(t, out, "data: [DONE]")
	})
}

// TestProxyToInstance_LeavesNameEnginesAlone locks that the translation is
// inert for engines that already answer to the client's name — the same
// code path, no special-casing per provider.
func TestProxyToInstance_LeavesNameEnginesAlone(t *testing.T) {
	var seen string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		seen = req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"org/model","choices":[]}`))
	}))
	t.Cleanup(engine.Close)

	s := createTestNodeWithDefaults(t)
	rec := httptest.NewRecorder()
	const body = `{"model":"org/model","messages":[]}`
	req := requestWithBody(t, body)
	s.proxyToInstance(rec, req, newInstanceRequest(req, []byte(body)),
		instanceFor(t, engine, "org/model", "org/model"), true)

	assert.Equal(t, "org/model", seen)

	var resp struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "org/model", resp.Model)
}
