package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// Pins the surface-aware error envelope contract: /api/* keeps the flat
// Ollama shape ({"error":"<msg>"}); /v1/* keeps the OpenAI closed-vocab
// envelope. The three normalize sites — ProxyClient.ForwardToBackend,
// proxyToInstance ModifyResponse, and Server.proxyToRemoteNode — all
// dispatch through the request dialect, so a regression at any one site
// shows up here.

func TestOllamaSurfaceErrorBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		raw    string
		want   string // exact JSON; "" means "match status placeholder"
	}{
		{
			name:   "already flat string error — passthrough byte-for-byte",
			status: 404,
			raw:    `{"error":"model 'foo' not found"}`,
			want:   `{"error":"model 'foo' not found"}`,
		},
		{
			name:   "object-form with message — unwrap to flat",
			status: 500,
			raw:    `{"error":{"message":"internal pipeline failure","type":"server_error","code":"backend_error"}}`,
			want:   `{"error":"internal pipeline failure"}`,
		},
		{
			name:   "object-form with nested error string — unwrap to flat",
			status: 502,
			raw:    `{"error":{"error":"upstream connection refused"}}`,
			want:   `{"error":"upstream connection refused"}`,
		},
		{
			name:   "object-form with no message field — sanitize whole body",
			status: 500,
			raw:    `{"error":{"foo":"bar"}}`,
			want:   `{"error":"{\"error\":{\"foo\":\"bar\"}}"}`,
		},
		{
			// Pins behavior if the inner struct ever becomes Message any.
			// Today numeric message fails the typed Unmarshal and falls
			// through to sanitize; a future struct change to `any` would
			// silently flip behavior unless this case fails first.
			name:   "object-form with numeric message — fails typed parse, sanitizes",
			status: 500,
			raw:    `{"error":{"message":123}}`,
			want:   `{"error":"{\"error\":{\"message\":123}}"}`,
		},
		{
			name:   "non-JSON body — sanitize whole body",
			status: 503,
			raw:    `Service Unavailable`,
			want:   `{"error":"Service Unavailable"}`,
		},
		{
			name:   "empty body — synthesize from status",
			status: 504,
			raw:    "",
			want:   `{"error":"backend returned status 504"}`,
		},
		{
			name:   "whitespace-only body — synthesize from status",
			status: 502,
			raw:    "   \n\t  ",
			want:   `{"error":"backend returned status 502"}`,
		},
		{
			name:   "OpenAI-shape envelope from non-Ollama backend — unwrap to flat",
			status: 401,
			raw:    `{"error":{"message":"invalid api key","type":"authentication_error","code":"invalid_api_key"}}`,
			want:   `{"error":"invalid api key"}`,
		},
		{
			name:   "leading/trailing whitespace around flat — passthrough preserves whitespace",
			status: 404,
			raw:    "  " + `{"error":"x"}` + "  ",
			want:   "  " + `{"error":"x"}` + "  ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaSurfaceErrorBody(tc.status, []byte(tc.raw))
			assert.Equal(t, tc.want, string(got))

			// Result must always be a valid {"error":"<string>"} envelope.
			var probe map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(got), &probe),
				"output must be valid JSON object")
			require.Contains(t, probe, "error")
			require.NotEmpty(t, probe["error"])
			assert.Equal(t, byte('"'), probe["error"][0],
				"top-level error value must be a string, got %s", string(probe["error"]))
		})
	}
}

// TestUpstreamErrorFollowsDialect pins that a backend error is rewritten
// in the dialect the route group attached, not one guessed from the path.
func TestUpstreamErrorFollowsDialect(t *testing.T) {
	// An object-form upstream body reveals which dialect ran: Ollama
	// unwraps to flat, OpenAI keeps the envelope shape.
	objForm := []byte(`{"error":{"message":"upstream said no","type":"server_error","code":"backend_error"}}`)

	cases := []struct {
		name       string
		responder  httperr.Responder
		wantOllama bool
	}{
		{"no responder speaks OpenAI", nil, false},
		{"openai responder", newResponderSet().openai, false},
		{"ollama responder", newOllamaResponder(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/any", nil)
			if tc.responder != nil {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = req
				httperr.AttachResponder(tc.responder)(c)
				req = c.Request
			}
			out := dialectOf(req).NormalizeUpstreamError(500, objForm)

			var probe map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out, &probe))
			require.Contains(t, probe, "error")
			want := byte('{')
			if tc.wantOllama {
				want = '"'
			}
			assert.Equal(t, want, probe["error"][0], "got %s", string(out))
		})
	}
}

// stubRoundTripper returns a canned response for every request. Lets us
// drive ProxyClient.ForwardToBackend without a live backend — the
// streamClient's Transport is the only injection seam needed.
type stubRoundTripper struct {
	status   int
	body     []byte
	encoding string // Content-Encoding header value (e.g. "gzip"); empty for plain
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	h := http.Header{"Content-Type": []string{"application/json"}}
	if s.encoding != "" {
		h.Set("Content-Encoding", s.encoding)
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Request:    req,
	}, nil
}

// routedRequest attaches the responder the request's route group would,
// as the server's path dispatcher maps it, for tests that call a proxy
// site directly instead of through the engine.
func routedRequest(req *http.Request) *http.Request {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	httperr.AttachResponder(newResponderSet().dispatcher.For(req.URL.Path))(c)
	return c.Request
}

// TestForwardToBackend_SurfaceShapes pins both surfaces against the
// SAME upstream object-form error body. Without this test, a refactor
// can silently flip /api/* to OpenAI-shape (or vice versa) and only
// the live smoke-test catches it.
func TestForwardToBackend_SurfaceShapes(t *testing.T) {
	// Object-form upstream — the case that distinguishes lanes.
	upstream := `{"error":{"message":"model 'smollm:135m' not found","type":"not_found_error","code":"backend_error"}}`

	pc := NewProxyClient(
		&http.Client{Transport: &stubRoundTripper{status: 404, body: []byte(upstream)}},
		nil, nil, nil, nil, nil,
	)

	cases := []struct {
		name        string
		path        string
		assertShape func(t *testing.T, body []byte)
	}{
		{
			name: "/api/chat — flat Ollama envelope, message unwrapped",
			path: "/api/chat",
			assertShape: func(t *testing.T, body []byte) {
				var probe map[string]any
				require.NoError(t, json.Unmarshal(body, &probe))
				msg, ok := probe["error"].(string)
				require.True(t, ok, "/api/* must produce string error, got %T %v", probe["error"], probe["error"])
				assert.Equal(t, "model 'smollm:135m' not found", msg)
			},
		},
		{
			name: "/v1/chat/completions — OpenAI envelope preserved",
			path: "/v1/chat/completions",
			assertShape: func(t *testing.T, body []byte) {
				var probe struct {
					Error struct {
						Message string  `json:"message"`
						Type    string  `json:"type"`
						Code    *string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &probe))
				assert.Equal(t, "model 'smollm:135m' not found", probe.Error.Message)
				assert.NotEmpty(t, probe.Error.Type, "OpenAI envelope must carry type")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := routedRequest(httptest.NewRequest(http.MethodPost, tc.path,
				bytes.NewReader([]byte(`{"model":"smollm:135m","messages":[]}`))))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			pc.ForwardToBackend(w, req, "ollama", "http://127.0.0.1:0"+tc.path, backend.Engine(), []byte(`{"model":"smollm:135m"}`))

			require.Equal(t, http.StatusNotFound, w.Code,
				"upstream 4xx must propagate verbatim, got %d body=%s", w.Code, w.Body.String())
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			assert.Equal(t, fmt.Sprint(len(w.Body.Bytes())), w.Header().Get("Content-Length"),
				"Content-Length must match rewritten body")
			tc.assertShape(t, w.Body.Bytes())
		})
	}
}

// TestForwardToBackend_GzippedObjectForm pins the full pipeline
// (readErrorBody + dialect rewrite + write) end-to-end against a
// gzipped object-form upstream — the case where readErrorBody must
// gunzip first and then the rewriter unwraps the inner message. Pre-
// F5 fix this path produced binary line noise as the user-facing
// error string. The helper-level test in TestReadErrorBody covers
// readErrorBody alone; this case proves ForwardToBackend wires the
// helper into the chain in the right order.
func TestForwardToBackend_GzippedObjectForm(t *testing.T) {
	upstream := `{"error":{"message":"upstream gateway timeout","type":"timeout"}}`
	gz := gzipBytes(t, upstream)

	pc := NewProxyClient(
		&http.Client{Transport: &stubRoundTripper{status: 504, body: gz, encoding: "gzip"}},
		nil, nil, nil, nil, nil,
	)

	req := routedRequest(httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"x"}`))))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	pc.ForwardToBackend(w, req, "ollama", "http://127.0.0.1:0/api/chat", backend.Engine(), []byte(`{"model":"x"}`))

	require.Equal(t, http.StatusGatewayTimeout, w.Code)
	var probe map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))
	msg, ok := probe["error"].(string)
	require.True(t, ok, "/api/* must produce string error, got %T", probe["error"])
	assert.Equal(t, "upstream gateway timeout", msg)

	// Content-Encoding must NOT be re-published — readErrorBody already
	// gunzipped, so the body coming out is plaintext JSON.
	assert.Empty(t, w.Header().Get("Content-Encoding"),
		"upstream Content-Encoding must not survive the rewrite")
}

// TestProxyToRemoteNode_SurfaceShapes pins the third call site against
// the SAME upstream object-form 404. Closes Gap #2 from the /api/*
// unification arc: pre-fix, /api/chat for a worker-resident model
// returned the OpenAI-shape envelope because proxyToRemoteNode hadn't
// adopted the surface-aware rewriter yet.
func TestProxyToRemoteNode_SurfaceShapes(t *testing.T) {
	upstream := `{"error":{"message":"model 'smollm:135m' not found","type":"not_found_error","code":"backend_error"}}`

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(upstream))
	}))
	t.Cleanup(remote.Close)

	s := createTestNodeWithDefaults(t)
	// remote.URL is the test worker's URL — pass it directly to the
	// URL-variant so the test exercises the proxy half without needing
	// a real cluster registry. host is a symbolic name used only in logs.
	const host = "test-worker"
	baseURL := remote.URL

	cases := []struct {
		name        string
		path        string
		assertShape func(t *testing.T, body []byte)
	}{
		{
			name: "/api/chat — flat Ollama envelope, message unwrapped",
			path: "/api/chat",
			assertShape: func(t *testing.T, body []byte) {
				var probe map[string]any
				require.NoError(t, json.Unmarshal(body, &probe))
				msg, ok := probe["error"].(string)
				require.True(t, ok, "/api/* must produce string error, got %T %v", probe["error"], probe["error"])
				assert.Equal(t, "model 'smollm:135m' not found", msg)
			},
		},
		{
			name: "/v1/chat/completions — OpenAI envelope preserved",
			path: "/v1/chat/completions",
			assertShape: func(t *testing.T, body []byte) {
				var probe struct {
					Error struct {
						Message string `json:"message"`
						Type    string `json:"type"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &probe))
				assert.Equal(t, "model 'smollm:135m' not found", probe.Error.Message)
				assert.NotEmpty(t, probe.Error.Type, "OpenAI envelope must carry type")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"smollm:135m","messages":[]}`)
			req := routedRequest(httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body)))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			s.proxyToWorkerURL(c, baseURL, host, "ollama", body, http.DefaultClient)

			require.Equal(t, http.StatusNotFound, w.Code,
				"upstream 4xx must propagate verbatim, got %d body=%s", w.Code, w.Body.String())
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			tc.assertShape(t, w.Body.Bytes())
		})
	}
}

// gzipBytes is a test helper that wraps a body in gzip, mirroring
// what an upstream that ignores Accept-Encoding:identity might emit.
func gzipBytes(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(raw))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestApplyRewrittenErrorGzip(t *testing.T) {
	t.Run("end-to-end with rewriter — gzipped object-form unwraps to flat", func(t *testing.T) {
		// Pre-fix: the rewriter would see compressed bytes, fail
		// the structural fingerprint, and stringify the binary blob.
		// Post-fix: readErrorBody gunzips first, the rewriter sees
		// plaintext object-form, unwraps to flat.
		gzipped := gzipBytes(t,
			`{"error":{"message":"upstream said no","type":"server_error"}}`)
		resp := &http.Response{
			Header: http.Header{"Content-Encoding": []string{"gzip"}},
			Body:   io.NopCloser(bytes.NewReader(gzipped)),
		}
		resp.StatusCode = 500
		applyRewrittenError(routedRequest(httptest.NewRequest("POST", "/api/chat", nil)), resp)
		out, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var probe map[string]any
		require.NoError(t, json.Unmarshal(out, &probe))
		msg, ok := probe["error"].(string)
		require.True(t, ok)
		assert.Equal(t, "upstream said no", msg)
	})
}

func TestUnsupportedImageProxyPaths(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"image input is not supported","type":"api_error"}}`))
	}))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/api/chat"} {
		for _, method := range []string{"direct", "instance", "worker"} {
			t.Run(path+method, func(t *testing.T) {
				body := []byte(`{"model":"org/model","stream":true}`)
				req := routedRequest(httptest.NewRequest("POST", path, bytes.NewReader(body)))
				w := httptest.NewRecorder()
				switch method {
				case "direct":
					s.proxy.ForwardToBackend(w, req, "mlx", engine.URL+path, backend.Engine(), body)
				case "instance":
					s.proxyToInstance(w, req, instanceFor(t, engine, "org/model", ""), true)
				case "worker":
					c, _ := gin.CreateTestContext(w)
					c.Request = req
					s.proxyToWorkerURL(c, engine.URL, "worker", "mlx", body, http.DefaultClient)
				}
				require.Equal(t, 400, w.Code, w.Body.String())
				assert.Empty(t, w.Header().Get("Retry-After"))
				assert.Contains(t, w.Body.String(), "image input is not supported")
				if path == "/api/chat" {
					assert.JSONEq(t, `{"error":"image input is not supported"}`, w.Body.String())
				} else {
					assert.Contains(t, w.Body.String(), `"type":"invalid_request_error"`)
				}
			})
		}
	}
}
