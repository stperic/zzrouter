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

type loadResultRouter struct {
	fakeRouter
	status int
	body   string
}

func (r *loadResultRouter) Unicast(context.Context, string, string, string, []byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: r.status, Body: []byte(r.body), Node: "worker"}, nil
}
func TestLoadServicePreservesWorkerErrorsAndWarmReuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		reusable bool
	}{
		{"admission conflict", 409, `{"type":"https://api.zzrouter.com/problems/conflict","status":409,"title":"Conflict","detail":"model name conflict","code":"conflict"}`, false},
		{"missing weights", 404, `{"error":"Model file not found","details":"download weights first"}`, false},
		{"bad provider", 400, `{"status":400,"title":"Bad Request","detail":"unknown provider"}`, false},
		{"upstream failure", 500, `{"status":500,"title":"Internal Server Error","detail":"load failed"}`, false},
		{"malformed success", 200, `not-json`, false},
		{"warm reuse", 409, `{"instance_id":"warm-run","model":"base","provider":"llamacpp","status":"running","port":8080,"job_id":"existing-job","error":"already running"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := &loadResultRouter{status: tc.status, body: tc.body}
			service := NewLoadService(nil, router)
			response, err := service.LoadModel(t.Context(), &LoadModelRequest{Node: "worker", Provider: "llamacpp", ModelName: "fast", Force: !tc.reusable})
			if tc.reusable {
				require.NoError(t, err)
				require.NotNil(t, response)
				assert.Equal(t, "warm-run", response.InstanceID)
				assert.Equal(t, "existing-job", response.JobID)
				assert.Equal(t, http.StatusConflict, response.StatusCode)
				assert.Equal(t, "worker", response.Node)
				return
			}
			require.Error(t, err)
			assert.Nil(t, response)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/zzrouter/v1/runs/load", nil)
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
