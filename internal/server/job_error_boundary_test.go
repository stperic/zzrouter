package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jobErrorRequest(router http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func assertJobProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code, node, instance, requestID string) utils.ProblemDetails {
	t.Helper()
	require.Equal(t, status, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, status, problem.Status)
	assert.Equal(t, code, problem.Code)
	assert.Equal(t, node, problem.Node)
	assert.Equal(t, instance, problem.Instance)
	assert.Equal(t, requestID, problem.RequestID)
	assert.NotEmpty(t, problem.Type)
	assert.NotEmpty(t, problem.Title)
	return problem
}

func jobErrorRouter(c *JobsController, internal bool, requestID string) *gin.Engine {
	router := gin.New()
	router.Use(func(ctx *gin.Context) { ctx.Set("request_id", requestID) })
	if internal {
		c.RegisterInternalRoutes(router.Group("/zzrouter/v1/internal"))
	} else {
		c.RegisterPublicRoutes(router.Group("/zzrouter/v1"))
	}
	return router
}

func TestJobErrorMissingOnWorkerAndExplicitPeer(t *testing.T) {
	worker := NewJobsController(newTestJobsRegistry(t), "w1", nil)
	internal := jobErrorRouter(worker, true, "worker-request")
	peer := httptest.NewServer(internal)
	t.Cleanup(peer.Close)
	public := jobErrorRouter(newProxyController(peer.URL, false), false, "public-request")
	for _, tc := range []struct{ name, method, suffix string }{
		{"get", http.MethodGet, ""},
		{"cancel", http.MethodPost, "/cancel"},
		{"stream", http.MethodGet, "/stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			internalPath := "/zzrouter/v1/internal/jobs/missing" + tc.suffix
			problem := assertJobProblem(t, jobErrorRequest(internal, tc.method, internalPath), 404, "job_not_found", "w1", internalPath, "worker-request")
			assert.NotContains(t, problem.Detail, "evicted 5m0s after completion")
			method, suffix := tc.method, tc.suffix
			if tc.name == "cancel" {
				method, suffix = http.MethodDelete, ""
			}
			publicPath := "/zzrouter/v1/jobs/missing" + suffix
			rec := jobErrorRequest(public, method, publicPath+"?node=w1")
			assertJobProblem(t, rec, 404, "job_not_found", "w1", publicPath, "public-request")
			assert.Empty(t, rec.Header().Get("X-Accel-Buffering"))
			assert.Empty(t, rec.Header().Get("Cache-Control"))
		})
	}
}

func TestJobErrorOwnerDisappearsAfterProbe(t *testing.T) {
	for _, tc := range []struct{ name, method, suffix string }{{"get", http.MethodGet, ""}, {"cancel", http.MethodDelete, ""}, {"stream", http.MethodGet, "/stream"}} {
		t.Run(tc.name, func(t *testing.T) {
			worker := jobErrorRouter(NewJobsController(newTestJobsRegistry(t), "w1", nil), true, "worker-request")
			var probes atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/zzrouter/v1/internal/jobs/gone" && probes.Add(1) == 1 {
					_, _ = fmt.Fprint(w, `{"job_id":"gone","node":"w1"}`)
					return
				}
				worker.ServeHTTP(w, r)
			}))
			t.Cleanup(peer.Close)
			c := newJobLookupController(t, map[string]string{"w1": peer.URL})
			router := jobErrorRouter(c, false, "public-request")
			path := "/zzrouter/v1/jobs/gone" + tc.suffix
			assertJobProblem(t, jobErrorRequest(router, tc.method, path), 404, "job_not_found", "w1", path, "public-request")
		})
	}
}

func TestJobErrorUpstreamRefusalsStayProblems(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"complete", `{"title":"Unavailable","type":"urn:worker:busy","status":503,"code":"registry_stopped","node":"w1","instance":"/internal/path","request_id":"worker-request","errors":[{"key":"job","code":"busy","message":"retry"}]}`, "registry_stopped"},
		{"title only", `{"title":"Unavailable"}`, "service_unavailable"},
		{"wrong embedded status", `{"title":"Unavailable","status":200,"code":"registry_stopped"}`, "registry_stopped"},
		{"empty old peer", "", "service_unavailable"},
		{"html old peer", "<html>unavailable</html>", "service_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(peer.Close)
			router := jobErrorRouter(newProxyController(peer.URL, false), false, "public-request")
			for _, path := range []string{"/zzrouter/v1/jobs", "/zzrouter/v1/jobs/missing", "/zzrouter/v1/jobs/missing/stream"} {
				rec := jobErrorRequest(router, http.MethodGet, path+"?node=w1")
				node := ""
				if tc.name == "complete" {
					node = "w1"
				}
				problem := assertJobProblem(t, rec, 503, tc.code, node, path, "public-request")
				assert.Equal(t, "5", rec.Header().Get("Retry-After"))
				if tc.name == "complete" {
					assert.Equal(t, "urn:worker:busy", problem.Type)
					require.Len(t, problem.Errors, 1)
					assert.Equal(t, "busy", problem.Errors[0].Code)
				}
			}
		})
	}
}

func TestJobErrorLegacy404DoesNotInventJobAbsence(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(peer.Close)
	path := "/zzrouter/v1/jobs/missing"
	router := jobErrorRouter(newProxyController(peer.URL, false), false, "public-request")
	assertJobProblem(t, jobErrorRequest(router, http.MethodGet, path+"?node=w1"), 404, "not_found", "", path, "public-request")
}

func TestJobErrorSuccessfulShapeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"malformed snapshot", http.MethodGet, "/zzrouter/v1/jobs/x", "not json", 200},
		{"empty snapshot", http.MethodGet, "/zzrouter/v1/jobs/x", `{}`, 200},
		{"identity only", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x"}`, 200},
		{"missing node", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x","kind":"download","phase":"done"}`, 200},
		{"missing kind", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x","node":"w1","phase":"done"}`, 200},
		{"unknown phase", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x","node":"w1","kind":"download","phase":"other"}`, 200},
		{"wrong owner", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x","node":"w2","kind":"download","phase":"done"}`, 200},
		{"oversized snapshot", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x","padding":"` + strings.Repeat("x", upstreamBodyLimit) + `"}`, 200},
		{"malformed list", http.MethodGet, "/zzrouter/v1/jobs", `{}`, 200},
		{"incomplete list entry", http.MethodGet, "/zzrouter/v1/jobs", `{"node":"w1","jobs":[{}]}`, 200},
		{"wrong list owner", http.MethodGet, "/zzrouter/v1/jobs", `{"node":"w2","jobs":[]}`, 200},
		{"wrong list entry owner", http.MethodGet, "/zzrouter/v1/jobs", `{"node":"w1","jobs":[{"job_id":"x","node":"w2","kind":"download","phase":"done"}]}`, 200},
		{"wrong snapshot status", http.MethodGet, "/zzrouter/v1/jobs/x", `{"job_id":"x"}`, 202},
		{"wrong cancel status", http.MethodDelete, "/zzrouter/v1/jobs/x", `{"accepted":true}`, 200},
		{"refused acceptance", http.MethodDelete, "/zzrouter/v1/jobs/x", `{"accepted":false}`, 202},
		{"malformed acceptance", http.MethodDelete, "/zzrouter/v1/jobs/x", "not json", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(peer.Close)
			router := jobErrorRouter(newProxyController(peer.URL, false), false, "public-request")
			assertJobProblem(t, jobErrorRequest(router, tc.method, tc.path+"?node=w1"), 502, "upstream_shape", "w1", tc.path, "public-request")
		})
	}
}

func TestJobErrorProxyAcceptsCanonicalNodeThroughAlias(t *testing.T) {
	for _, path := range []string{"/zzrouter/v1/jobs/x", "/zzrouter/v1/jobs"} {
		t.Run(path, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.URL.Query().Get("node"))
				ev := `{"job_id":"x","node":"w1","kind":"download","phase":"done"}`
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					ev = `{"node":"w1","jobs":[` + ev + `]}`
				}
				_, _ = io.WriteString(w, ev)
			}))
			t.Cleanup(peer.Close)
			c := newProxyController(peer.URL, false)
			var resolutions atomic.Int32
			c.proxy.ResolveEndpoint = func(node string) *mesh.Endpoint {
				assert.Equal(t, "alias", node)
				resolutions.Add(1)
				return &mesh.Endpoint{NodeName: "w1", ClusterURL: peer.URL}
			}
			rec := jobErrorRequest(jobErrorRouter(c, false, "public-request"), http.MethodGet, path+"?node=alias")
			require.Equal(t, http.StatusOK, rec.Code)
			assert.EqualValues(t, 1, resolutions.Load(), "identity comes from the existing endpoint resolution")
			assert.Contains(t, rec.Body.String(), `"node":"w1"`)
		})
	}
}

type jobErrorBody struct{ err error }

func (r jobErrorBody) Read([]byte) (int, error) { return 0, r.err }

func (r jobErrorBody) Close() error { return nil }

func TestJobErrorBodyCancellationIsSilent(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := newProxyController("http://unused", false)
			c.proxy.StreamClient = func() (*http.Client, error) {
				return &http.Client{Transport: jobProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: jobErrorBody{context.Canceled}, Request: req}, nil
				})}, nil
			}
			paths := []string{"/zzrouter/v1/jobs/x", "/zzrouter/v1/jobs"}
			if status != http.StatusOK {
				paths = append(paths, "/zzrouter/v1/jobs/x/stream")
			}
			for _, path := range paths {
				rec := jobErrorRequest(jobErrorRouter(c, false, "public-request"), http.MethodGet, path+"?node=w1")
				assert.Empty(t, rec.Body.String())
				assert.Empty(t, rec.Header().Get("Content-Type"))
			}
		})
	}
}

func TestJobErrorRefusalReadFailurePreservesStatusAndBackoff(t *testing.T) {
	c := newProxyController("http://unused", false)
	c.proxy.StreamClient = func() (*http.Client, error) {
		return &http.Client{Transport: jobProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
			body := io.MultiReader(strings.NewReader(`{"title":"Unavailable","code":"untrusted_partial"}`), jobErrorBody{io.ErrUnexpectedEOF})
			return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": {"5"}}, Body: io.NopCloser(body), Request: req}, nil
		})}, nil
	}
	path := "/zzrouter/v1/jobs/x"
	rec := jobErrorRequest(jobErrorRouter(c, false, "public-request"), http.MethodGet, path+"?node=w1")
	assertJobProblem(t, rec, 503, "service_unavailable", "", path, "public-request")
	assert.Equal(t, "5", rec.Header().Get("Retry-After"))
}

func TestJobErrorTransportIsSanitizedAndCancellationSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"unreachable", errors.New("private implementation pkg/internal.go:42 at 192.0.2.8:9443"), string(httperr.CodeNodeUnreachable)},
		{"timeout", context.DeadlineExceeded, string(httperr.CodeUpstreamTimeout)},
		{"canceled", context.Canceled, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newProxyController("http://unused", false)
			c.proxy.StreamClient = func() (*http.Client, error) { return nil, tc.err }
			router := jobErrorRouter(c, false, "public-request")
			path := "/zzrouter/v1/jobs/x"
			rec := jobErrorRequest(router, http.MethodGet, path+"?node=w1")
			if tc.code == "" {
				assert.Empty(t, rec.Body.String())
				return
			}
			problem := assertJobProblem(t, rec, 502, tc.code, "w1", path, "public-request")
			assert.NotContains(t, problem.Detail, tc.err.Error())
		})
	}
}
