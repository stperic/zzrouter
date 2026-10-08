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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

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
	inst.MarkRunning()
	return inst
}

func requestWithBody(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	ctx := context.WithValue(req.Context(), CtxKeyOriginalBody, []byte(body))
	ctx = context.WithValue(ctx, CtxKeyModel, "org/model")
	return req.WithContext(ctx)
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
		s.proxyToInstance(rec, requestWithBody(t, `{"model":"org/model","messages":[]}`),
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
		s.proxyToInstance(rec, requestWithBody(t, `{"model":"org/model","stream":true,"messages":[]}`),
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
	s.proxyToInstance(rec, requestWithBody(t, `{"model":"org/model","messages":[]}`),
		instanceFor(t, engine, "org/model", "org/model"), true)

	assert.Equal(t, "org/model", seen)

	var resp struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "org/model", resp.Model)
}
