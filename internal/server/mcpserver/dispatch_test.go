package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const testKey = "mx-zzr-mcp-caller-Kp9vB4xRmT2LqJfNdY"

func testDispatcher(handler http.HandlerFunc) dispatcher {
	return dispatcher{cfg: Config{Engine: handler, KeyHeaders: []string{"X-API-Key", "Authorization"}, Streaming: func(_, _ string) string { return "" }}, outer: &outerRequest{header: make(http.Header), remoteAddr: "198.51.100.8:47821", requestID: "outer", ctx: context.Background()}}
}

func TestDispatchContextIsolation(t *testing.T) {
	type marker struct{}
	parent, cancel := context.WithTimeout(context.WithValue(context.Background(), marker{}, true), time.Minute)
	defer cancel()
	deadline, _ := parent.Deadline()
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		require.Nil(t, r.Context().Value(marker{}), "outer authentication value leaked")
		require.True(t, IsDispatch(r.Context()))
		got, ok := r.Context().Deadline()
		require.True(t, ok)
		require.Equal(t, deadline, got)
		cancel()
		<-r.Context().Done()
		require.ErrorIs(t, r.Context().Err(), context.Canceled)
	})
	var out any
	require.ErrorIs(t, d.call(parent, request{method: "GET", path: routePrefix("/test").path()}, &out), context.Canceled)
}

func TestDispatchRemoteAddr(t *testing.T) {
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "198.51.100.8:47821", r.RemoteAddr)
		// Mirrors the loopback-or-admin risk: a remote caller without a key is refused.
		if strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") || r.Header.Get("X-API-Key") != "" {
			writeFixture(t, w, []byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	var out any
	require.ErrorContains(t, d.call(context.Background(), request{method: "POST", path: routePrefix("/zzrouter/v1/cluster/pair").path()}, &out), "HTTP 401")
}

func TestDispatchHeaderAllowlist(t *testing.T) {
	dHeaderForwarded := []string{"203.0.113.1, 198.51.100.3", "198.51.100.4"}
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, testKey, r.Header.Get("X-API-Key"))
		require.Equal(t, "Bearer "+testKey, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-Cluster-API-Key"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Equal(t, dHeaderForwarded, r.Header.Values("X-Forwarded-For"))
		require.Equal(t, []string{"203.0.113.2"}, r.Header.Values("X-Real-IP"))
		require.Equal(t, "application/merge-patch+json", r.Header.Get("Content-Type"))
		require.Equal(t, "retry-1", r.Header.Get("Idempotency-Key"))
		require.Equal(t, "revision-1", r.Header.Get("If-Match"))
		require.Equal(t, "outer.1", r.Header.Get("X-Request-ID"))
		writeFixture(t, w, []byte(`{}`))
	})
	d.outer.header = http.Header{"X-Api-Key": {testKey}, "Authorization": {"Bearer " + testKey}, "X-Cluster-Api-Key": {testKey}, "Cookie": {"secret=1"}, "X-Forwarded-For": dHeaderForwarded, "X-Real-Ip": {"203.0.113.2"}, "Content-Type": {"text/plain"}, "Idempotency-Key": {"outer-only"}, "If-Match": {"outer-only"}}
	var out any
	require.NoError(t, d.call(context.Background(), request{method: "PATCH", path: routePrefix("/test").path(), contentType: "application/merge-patch+json", idempotencyKey: "retry-1", ifMatch: "revision-1"}, &out))
	// Caller headers alone cannot install concurrency or retry conditions.
	d.cfg.Engine = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Idempotency-Key"))
		require.Empty(t, r.Header.Get("If-Match"))
		require.Equal(t, "outer.2", r.Header.Get("X-Request-ID"))
		writeFixture(t, w, []byte(`{}`))
	})
	require.NoError(t, d.call(context.Background(), request{method: "GET", path: routePrefix("/test").path()}, &out))
}

func TestDispatchStreamingRefused(t *testing.T) {
	called := false
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) { called = true; <-r.Context().Done() })
	d.cfg.Streaming = func(method, path string) string {
		require.Equal(t, "GET", method)
		require.Equal(t, "/zzrouter/v1/jobs/abc/stream", path)
		return "sse"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out any
	require.ErrorContains(t, d.call(ctx, request{method: "GET", path: routePrefix("/zzrouter/v1/jobs/abc/stream").path()}, &out), "streaming route")
	require.False(t, called)
}

func TestDispatchErrorsAndCap(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, want string
		status                        int
	}{
		{"problem", `{"code":"bad_parameter","detail":"Fix provider parameters","errors":[{"key":"ctx","message":"too large"},{"key":"gpu","message":"unknown"}]}`, "application/problem+json; charset=utf-8", "HTTP 400 bad_parameter: Fix provider parameters\nctx: too large\ngpu: unknown", 400},
		{"excerpt", strings.Repeat("x", errorExcerptBytes+20), "text/plain", "HTTP 502: " + strings.Repeat("x", errorExcerptBytes), 502},
		{"large problem", `{"code":"bad_parameter","detail":"Fix provider parameters"}` + strings.Repeat(" ", MaxResponseBytes), "application/problem+json", "HTTP 400 bad_parameter: Fix provider parameters", 400},
		{"large excerpt", strings.Repeat("x", MaxResponseBytes+1), "text/plain", "HTTP 502: " + strings.Repeat("x", errorExcerptBytes), 502},
		{"cap", strings.Repeat("x", MaxResponseBytes+1), "application/json", "REST response exceeds the 1 MiB cap; narrow the query and retry", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				if len(tc.body) > MaxResponseBytes {
					require.ErrorContains(t, err, "response cap exceeded")
				} else {
					require.NoError(t, err)
				}
			})
			var out any
			err := d.call(context.Background(), request{method: "GET", path: routePrefix("/test").path()}, &out)
			require.EqualError(t, err, tc.want)
		})
	}
	t.Run("exact cap", func(t *testing.T) {
		d := testDispatcher(func(w http.ResponseWriter, _ *http.Request) {
			writeFixture(t, w, []byte(`"`+strings.Repeat("x", MaxResponseBytes-2)+`"`))
		})
		var out string
		require.NoError(t, d.call(context.Background(), request{method: "GET", path: routePrefix("/test").path()}, &out))
	})
}

func TestDispatchOuterCancellation(t *testing.T) {
	outer, cancel := context.WithCancel(context.Background())
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() })
	d.outer.ctx = outer
	var out any
	require.True(t, errors.Is(d.call(context.Background(), request{method: "GET", path: routePrefix("/test").path()}, &out), context.Canceled))
}

func TestJobSnapshotsAndWait(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		wait, calls int
	}{{"snapshot", "running", 0, 1}, {"terminal", "done", 45, 1}, {"failed", "failed", 45, 1}, {"poll", "done", 3, 2}, {"bounded", "running", 1, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/zzrouter/v1/jobs/abc", r.URL.Path)
				require.Equal(t, "worker one", r.URL.Query().Get("node"))
				calls++
				phase := tc.phase
				if tc.name == "poll" && calls == 1 {
					phase = "running"
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"job_id": "abc", "phase": phase, "percent": 60, "meta": map[string]any{"model": "small"}, "err": "provider failed"}}))
			})
			out, err := d.job(context.Background(), jobInput{JobID: "abc", Node: "worker one", WaitSeconds: tc.wait})
			require.NoError(t, err)
			require.Equal(t, tc.calls, calls)
			if tc.name == "bounded" {
				require.Equal(t, "still running, call zz_job again", string(out.State))
			} else {
				require.Equal(t, tc.phase, string(out.State))
			}
			require.Equal(t, 60, out.Progress)
			if tc.phase == "done" {
				require.Equal(t, "small", out.Result["model"])
			}
			if tc.phase == "failed" {
				require.Equal(t, "provider failed", out.Error)
			}
		})
	}
	for _, input := range []jobInput{{JobID: "../health"}, {JobID: "abc", WaitSeconds: -1}, {JobID: "abc", WaitSeconds: 46}} {
		d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input dispatched") })
		_, err := d.job(context.Background(), input)
		require.Error(t, err)
	}
}

func TestInstructionsBudget(t *testing.T) { require.Less(t, len(instructions), 2<<10) }

func TestOverviewCompactProviders(t *testing.T) {
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zzrouter/v1":
			writeFixture(t, w, []byte(`{"data":{"endpoint_groups":{"models":["GET /zzrouter/v1/models"],"mcp":["POST /zzrouter/v1/mcp"]},"chat_templates":"long prose"}}`))
		case "/zzrouter/v1/nodes":
			writeFixture(t, w, []byte(`{"data":[{"name":"coord","cluster_role":"coordinator","health_status":"healthy","providers":[{"key":"vllm","name":"vLLM","formats":["safetensors"]}]},{"name":"worker","cluster_role":"worker","health_status":"down","providers":[{"key":"vllm","name":"vLLM"},{"key":"ollama","name":"Ollama"}]}]}`))
		case "/zzrouter/v1/health":
			writeFixture(t, w, []byte(`{"data":{"cluster_health":"degraded","total_nodes":2}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	out, err := d.overview(context.Background(), overviewInput{})
	require.NoError(t, err)
	require.Equal(t, "degraded", out.Health)
	require.Len(t, out.Nodes, 2)
	require.Equal(t, "worker", out.Nodes[1].Name)
	require.Equal(t, "down", out.Nodes[1].Health)
	require.Equal(t, []string{"ollama", "vllm"}, out.EnabledProviders)
	require.Equal(t, []string{"vllm"}, out.Nodes[0].EnabledProviders)
	require.Equal(t, 2, out.EndpointCount)
	require.Equal(t, "/openapi.yaml", out.OpenAPI)
}

func TestJobInFlightWaitBound(t *testing.T) {
	calls := 0
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeFixture(t, w, []byte(`{"data":{"job_id":"abc","phase":"running"}}`))
			return
		}
		// A slow node must not extend the tool's advertised wait.
		<-r.Context().Done()
	})
	out, err := d.job(context.Background(), jobInput{JobID: "abc", WaitSeconds: 2})
	require.NoError(t, err)
	require.Equal(t, "abc", out.JobID)
	require.Equal(t, "still running, call zz_job again", string(out.State))
	require.Equal(t, 2, calls)
}

func writeFixture(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()
	_, err := w.Write(body)
	require.NoError(t, err)
}

func TestPathSegment(t *testing.T) {
	for _, id := range []string{"", ".", "..", "a/b", "a\\b", "a?b", "a#b", "a%b"} {
		t.Run(id, func(t *testing.T) {
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("unsafe id dispatched") })
			_, err := d.job(context.Background(), jobInput{JobID: id})
			require.ErrorContains(t, err, "job_id: invalid path identifier")
		})
	}
	id := "job +:é"
	path, err := pathSegment(jobsRoute, id)
	require.NoError(t, err)
	require.Equal(t, string(jobsRoute)+url.PathEscape(id), path.value)
	d := testDispatcher(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, string(jobsRoute)+id, r.URL.Path)
		require.Empty(t, r.URL.RawQuery)
		writeFixture(t, w, []byte(`{}`))
	})
	var out any
	require.NoError(t, d.call(context.Background(), request{method: "GET", path: path}, &out))
}

func TestDispatchClientIPAndFlush(t *testing.T) {
	engine := gin.New()
	headers := http.Header{"X-Forwarded-For": {"203.0.113.7, 198.51.100.8"}, "X-Real-Ip": {"203.0.113.9"}}
	outer := httptest.NewRequest("GET", "/test", nil)
	outer.Header, outer.RemoteAddr = headers, "198.51.100.8:47821"
	expected, _ := gin.CreateTestContext(httptest.NewRecorder())
	expected.Request = outer
	engine.GET("/test", func(c *gin.Context) {
		require.Equal(t, expected.ClientIP(), c.ClientIP())
		c.Writer.Flush()
		c.JSON(http.StatusOK, gin.H{})
	})
	d := testDispatcher(nil)
	d.cfg.Engine, d.outer.header = engine, headers
	var out any
	require.NoError(t, d.call(context.Background(), request{method: "GET", path: routePrefix("/test").path()}, &out))
}

func TestJobFirstPollWaitBound(t *testing.T) {
	d := testDispatcher(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	out, err := d.job(context.Background(), jobInput{JobID: "slow-job", WaitSeconds: 1})
	require.NoError(t, err)
	require.Equal(t, "slow-job", out.JobID)
	require.Equal(t, "still running, call zz_job again", string(out.State))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = d.job(ctx, jobInput{JobID: "slow-job", WaitSeconds: 1})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDispatcherMissingMetadata(t *testing.T) {
	for _, value := range []any{nil, true, (*outerRequest)(nil)} {
		ctx := context.Background()
		if value != nil {
			ctx = context.WithValue(ctx, outerKey, value)
		}
		_, err := newDispatcher(ctx, Config{})
		require.ErrorContains(t, err, "retry through the public MCP endpoint")
	}
}
