package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
)

func uploadLimitEngine(limit int64) *gin.Engine {
	engine := gin.New()
	engine.Use(httperr.AttachByPath(newResponderSet().dispatcher))
	engine.Use(requestSizeLimitMiddleware(limit))
	return engine
}

func ollamaUploadLimitIngress(t *testing.T, limit int64, upstream http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	daemon := httptest.NewServer(upstream)
	t.Cleanup(daemon.Close)
	service := &OllamaServiceImpl{
		ollamaDaemon: func() (*backend.Resolved, error) {
			return &backend.Resolved{Endpoint: daemon.URL, Upstream: backend.Engine()}, nil
		},
		httpStreamingClient: daemon.Client(),
	}
	engine := uploadLimitEngine(limit)
	calls := &atomic.Int32{}
	engine.POST("/api/blobs/:digest", func(c *gin.Context) {
		calls.Add(1)
		setAccessContext(c, adminResponseOwner())
		if err := service.CreateBlob(c, c.Param("digest")); err != nil && !c.Writer.Written() {
			RespondToErrorOllama(c, err)
		}
	})
	ingress := httptest.NewServer(engine)
	t.Cleanup(ingress.Close)
	return ingress, calls
}

func TestIntegrationOllamaUploadRequestLimits(t *testing.T) {
	const limit int64 = 32 << 10
	for _, tc := range []struct {
		name       string
		size       int64
		unknown    bool
		wantStatus int
	}{
		{"known oversized", limit + 1, false, http.StatusRequestEntityTooLarge},
		{"chunked oversized", limit + 1, true, http.StatusRequestEntityTooLarge},
		{"known exact limit", limit, false, http.StatusCreated},
		{"chunked exact limit", limit, true, http.StatusCreated},
		{"known small", 17, false, http.StatusCreated},
		{"chunked small", 17, true, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type observation struct {
				bytes, length int64
				contentType   string
				incomplete    bool
			}
			observed := make(chan observation, 1)
			var upstreamCalls atomic.Int32
			ingress, routeCalls := ollamaUploadLimitIngress(t, limit, func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				n, err := io.Copy(io.Discard, r.Body)
				observed <- observation{n, r.ContentLength, r.Header.Get("Content-Type"), err != nil}
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("X-Native-Blob", "accepted")
				w.WriteHeader(http.StatusCreated)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingress.URL+"/api/blobs/sha256:fixture", bytes.NewReader(bytes.Repeat([]byte("x"), int(tc.size))))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			if tc.unknown {
				req.ContentLength = -1
			}
			resp, err := ingress.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode, string(body))
			if tc.wantStatus == http.StatusRequestEntityTooLarge {
				assert.JSONEq(t, fmt.Sprintf(`{"error":"request body exceeds the %d-byte limit"}`, limit), string(body))
			} else {
				assert.Equal(t, "accepted", resp.Header.Get("X-Native-Blob"))
			}
			if !tc.unknown && tc.size > limit {
				assert.Zero(t, routeCalls.Load(), "known over-cap request must abort before the route")
				assert.Zero(t, upstreamCalls.Load(), "known over-cap request must not reach the daemon")
				return
			}
			assert.EqualValues(t, 1, routeCalls.Load())
			select {
			case got := <-observed:
				assert.EqualValues(t, 1, upstreamCalls.Load())
				assert.Equal(t, min(tc.size, limit), got.bytes)
				assert.Equal(t, tc.size > limit, got.incomplete)
				assert.Equal(t, "application/octet-stream", got.contentType)
				if tc.unknown {
					assert.EqualValues(t, -1, got.length)
				} else {
					assert.Equal(t, tc.size, got.length)
				}
			case <-ctx.Done():
				t.Fatal("daemon did not finish receiving the bounded request")
			}
		})
	}
}

func TestIntegrationOllamaUploadStreamsBeforeEOF(t *testing.T) {
	const limit int64 = 32 << 10
	started := make(chan struct{})
	observed := make(chan int64, 1)
	ingress, _ := ollamaUploadLimitIngress(t, limit, func(w http.ResponseWriter, r *http.Request) {
		first := make([]byte, 1)
		_, err := io.ReadFull(r.Body, first)
		if err != nil {
			observed <- -1
			return
		}
		close(started)
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			observed <- -1
			return
		}
		observed <- n + 1
		w.WriteHeader(http.StatusCreated)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingress.URL+"/api/blobs/sha256:fixture", reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := ingress.Client().Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		done <- result{status: resp.StatusCode, err: err}
	}()
	_, err = writer.Write([]byte("x"))
	require.NoError(t, err)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("upload was buffered before reaching the daemon")
	}
	_, err = writer.Write(bytes.Repeat([]byte("x"), int(limit/2)-1))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, http.StatusCreated, got.status)
	case <-ctx.Done():
		t.Fatal("streamed upload did not finish")
	}
	select {
	case n := <-observed:
		assert.Equal(t, limit/2, n)
	case <-ctx.Done():
		t.Fatal("daemon did not finish the stream")
	}
}

func TestIntegrationRequestLimitOrdinaryRoutesAndNestedReaders(t *testing.T) {
	const limit int64 = 1024
	for _, path := range []string{"/zzrouter/v1/ordinary", "/v1/chat/completions", "/v1/messages"} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s chunked=%t", path, unknown), func(t *testing.T) {
				engine := uploadLimitEngine(limit)
				var routes atomic.Int32
				engine.POST(path, func(c *gin.Context) {
					routes.Add(1)
					if _, ok := readRequestBody(c); ok {
						c.Status(http.StatusNoContent)
					}
				})
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", int(limit+1))))
				if unknown {
					request.ContentLength = -1
				}
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
				assert.Contains(t, recorder.Body.String(), fmt.Sprintf("%d-byte limit", limit))
				if !unknown {
					assert.Zero(t, routes.Load())
				}
				switch path {
				case "/zzrouter/v1/ordinary":
					assert.Contains(t, recorder.Body.String(), `"status":413`)
				case "/v1/chat/completions":
					assert.Contains(t, recorder.Body.String(), `"code":"request_too_large"`)
				case "/v1/messages":
					assert.Contains(t, recorder.Body.String(), `"type":"request_too_large"`)
				}
			})
		}
	}
	for _, kind := range []string{"opaque", "multipart"} {
		t.Run("nested "+kind, func(t *testing.T) {
			outerLimit := limit * 4
			engine := uploadLimitEngine(limit)
			s := &Server{config: &config.NodeConfig{}, responders: newResponderSet()}
			s.config.OpenAICompat.MaxUploadBytes = outerLimit
			engine.POST("/v1/upload", func(c *gin.Context) {
				if kind == "multipart" {
					_, _, _ = readOpenAIMultipartBody(c, outerLimit)
				} else {
					_, _ = s.readPassthroughBody(c)
				}
			})
			var body bytes.Buffer
			contentType := "application/json"
			if kind == "multipart" {
				writer := multipart.NewWriter(&body)
				part, err := writer.CreateFormFile("file", "fixture.bin")
				require.NoError(t, err)
				_, err = part.Write(bytes.Repeat([]byte("x"), int(limit+1)))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				contentType = writer.FormDataContentType()
			} else {
				_, _ = body.Write(bytes.Repeat([]byte("x"), int(limit+1)))
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/upload", &body)
			request.Header.Set("Content-Type", contentType)
			request.ContentLength = -1
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
			assert.Contains(t, recorder.Body.String(), fmt.Sprintf("%d bytes", limit))
			assert.NotContains(t, recorder.Body.String(), fmt.Sprintf("%d bytes", outerLimit))
		})
	}
}

func TestIntegrationRequestLimitCORSEngineOrdering(t *testing.T) {
	const limit int64 = 1024
	const allowed = "https://dashboard.example.test"
	const denied = "https://denied.example.test"
	for _, origin := range []string{allowed, denied} {
		for _, unknown := range []bool{false, true} {
			for _, oversized := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s chunked=%t oversized=%t", origin, unknown, oversized), func(t *testing.T) {
					cfg := &config.NodeConfig{}
					cfg.Node.MaxRequestSize = limit
					cfg.Security.CORS = config.CORSConfig{Enabled: true, AllowedOrigins: []string{allowed}}
					s := &Server{config: cfg, responders: newResponderSet()}
					s.initHTTPEngine(cfg)
					var called atomic.Int32
					s.engine.POST("/api/cors-limit", func(c *gin.Context) {
						called.Add(1)
						if _, ok := readRequestBody(c); ok {
							c.Status(http.StatusNoContent)
						}
					})
					size := limit / 2
					wantStatus := http.StatusNoContent
					if oversized {
						size = limit + 1
						wantStatus = http.StatusRequestEntityTooLarge
					}
					req := httptest.NewRequest(http.MethodPost, "/api/cors-limit", strings.NewReader(strings.Repeat("x", int(size))))
					req.Header.Set("Origin", origin)
					if unknown {
						req.ContentLength = -1
					}
					w := httptest.NewRecorder()
					s.engine.ServeHTTP(w, req)
					require.Equal(t, wantStatus, w.Code, w.Body.String())
					assert.Contains(t, w.Header().Values("Vary"), "Origin")
					if origin == allowed {
						assert.Equal(t, allowed, w.Header().Get("Access-Control-Allow-Origin"))
					} else {
						assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
					}
					if oversized && !unknown {
						assert.Zero(t, called.Load(), "known over-cap request must not invoke the route")
					} else {
						assert.EqualValues(t, 1, called.Load())
					}
				})
			}
		}
	}
}

func TestIntegrationRequestMetadataEngineBoundary(t *testing.T) {
	const limit int64 = 1024
	cfg := &config.NodeConfig{}
	cfg.Node.MaxRequestSize = limit
	manager, err := role.NewManager(role.RoleCoordinator)
	require.NoError(t, err)
	rig := newAccessRig(t)
	s := &Server{config: cfg, responders: newResponderSet(), role: manager}
	s.auth = &AuthHandlers{access: rig.access, config: cfg, responders: s.responders}
	s.initHTTPEngine(cfg)
	var atEntry string
	s.engine.Use(func(c *gin.Context) { atEntry = c.GetString("request_id"); c.Next() })
	s.engine.POST("/zzrouter/v1/limit", func(c *gin.Context) {
		if _, ok := readRequestBody(c); ok {
			c.JSON(http.StatusOK, gin.H{"request_id": c.GetString("request_id")})
		}
	})
	s.engine.GET("/health/metadata", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"request_id": c.GetString("request_id")}) })
	s.engine.POST("/zzrouter/v1/protected", s.auth.AuthMiddleware(RoleAdmin), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	s.registerMessagesRoutes()
	seen := map[string]bool{}
	for index, tc := range []struct {
		name, method, path, body string
		unknown                  bool
		status                   int
		problem                  bool
	}{
		{"known oversized", http.MethodPost, "/zzrouter/v1/limit", strings.Repeat("x", int(limit+1)), false, http.StatusRequestEntityTooLarge, true},
		{"chunked oversized", http.MethodPost, "/zzrouter/v1/limit", strings.Repeat("x", int(limit+1)), true, http.StatusRequestEntityTooLarge, true},
		{"authentication", http.MethodPost, "/zzrouter/v1/protected", "{}", false, http.StatusUnauthorized, true},
		{"not found", http.MethodGet, "/zzrouter/v1/missing", "", false, http.StatusNotFound, true},
		{"method not allowed", http.MethodGet, "/zzrouter/v1/limit", "", false, http.StatusMethodNotAllowed, true},
		{"normal management", http.MethodPost, "/zzrouter/v1/limit", "{}", false, http.StatusOK, true},
		{"normal health", http.MethodGet, "/health/metadata", "", false, http.StatusOK, true},
		{"compatibility group", http.MethodPost, "/v1/messages", "{", false, http.StatusBadRequest, false},
	} {
		for _, echo := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s echoed=%t", tc.name, echo), func(t *testing.T) {
				atEntry = ""
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				inputID := fmt.Sprintf("caller-%d", index)
				if echo {
					req.Header.Set("X-Request-ID", inputID)
				}
				if tc.unknown {
					req.ContentLength = -1
				}
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, req)
				require.Equal(t, tc.status, w.Code, w.Body.String())
				id := w.Header().Get("X-Request-ID")
				require.NotEmpty(t, id)
				assert.Equal(t, "v1", w.Header().Get("X-API-Version"))
				if echo {
					assert.Equal(t, inputID, id)
				} else {
					assert.False(t, seen[id], "each request needs its own ID")
					seen[id] = true
				}
				if atEntry != "" {
					assert.Equal(t, atEntry, id, "route group must preserve the ID minted at ingress")
				}
				if tc.problem {
					var body map[string]any
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
					assert.Equal(t, id, body["request_id"])
				}
			})
		}
	}
}
