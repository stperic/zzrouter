package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/routing"
)

type runResultRouter struct {
	fakeRouter
	status  int
	body    string
	headers http.Header
	node    string
}

func (r *runResultRouter) Unicast(context.Context, string, string, string, []byte) (*routing.Response, error) {
	node := r.node
	if node == "" {
		node = "worker"
	}
	return &routing.Response{StatusCode: r.status, Body: []byte(r.body), Headers: r.headers, Node: node}, nil
}
func (r *runResultRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	return r.Unicast(ctx, req.Node, req.Path, req.Method, req.Body)
}

func TestPreviewRunRequiresModelRouterForImplicitTarget(t *testing.T) {
	service := NewRunsService(&runResultRouter{status: http.StatusOK, body: `{"model":"m"}`})
	for _, node := range []string{"", "*"} {
		preview, err := service.PreviewRun(t.Context(), &PreviewRunRequest{Provider: "fake", ModelName: "m", Node: node})
		require.Nil(t, preview)
		problem := requireStartProblem(t, err, http.StatusServiceUnavailable, "service_unavailable")
		require.Contains(t, problem.Detail, "pass node")
	}
	preview, err := service.PreviewRun(t.Context(), &PreviewRunRequest{Provider: "fake", ModelName: "m", Node: "worker"})
	require.NoError(t, err)
	require.Equal(t, "m", preview.Model)
}

func TestRunsServicePreservesWorkerErrorsAndWarmReuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		reusable bool
	}{
		{"admission conflict", 409, `{"type":"https://api.zzrouter.com/problems/conflict","status":409,"title":"Conflict","detail":"model name conflict","code":"conflict"}`, false},
		{"missing weights", 404, `{"error":"Model file not found","details":"download weights first"}`, false},
		{"bad provider", 404, `{"status":404,"title":"Not Found","detail":"unknown provider"}`, false},
		{"upstream failure", 500, `{"status":500,"title":"Internal Server Error","detail":"load failed"}`, false},
		{"malformed success", 200, `not-json`, false},
		{"warm reuse", 202, `{"id":"warm-run","model":"base","provider":"llamacpp","status":"running","port":8080,"job_id":"existing-job","node":"worker"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := &runResultRouter{status: tc.status, body: tc.body}
			service := NewRunsService(router)
			response, err := service.LaunchRun(t.Context(), &LaunchRunRequest{Node: "worker", Provider: "llamacpp", Model: "fast", Replace: !tc.reusable})
			if tc.reusable {
				require.NoError(t, err)
				require.NotNil(t, response)
				assert.Equal(t, "warm-run", response.ID)
				assert.Equal(t, "existing-job", response.JobID)
				assert.Equal(t, "worker", response.Node)
				return
			}
			require.Error(t, err)
			assert.Nil(t, response)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/zzrouter/v1/runs", nil)
			RespondToError(c, err)
			want := tc.status
			if want < http.StatusBadRequest {
				want = http.StatusBadGateway
			}
			assert.Equal(t, want, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), `"success":true`)
		})
	}
}
