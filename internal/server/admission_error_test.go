package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

type admissionStreamRecorder struct {
	httperr.InBandStreamer
	failure httperr.Error
}

func (r *admissionStreamRecorder) StreamError(w http.ResponseWriter, e httperr.Error) {
	r.failure = e
	r.InBandStreamer.StreamError(w, e)
}

func TestAdmissionErrorWire(t *testing.T) {
	for _, reason := range []struct {
		code string
		wait time.Duration
	}{
		{"capacity_exceeded", 0},
		{"queue_wait_expired", 10 * time.Millisecond},
	} {
		t.Run(reason.code, func(t *testing.T) {
			inst := instance.NewInstance("id", "p", "m", 0, 0, 0)
			inst.InitConcurrencyLimit(instance.ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, QueueTimeout: reason.wait})
			held, err := inst.Acquire(context.Background(), nil)
			require.NoError(t, err)
			defer held.Release()
			s := &Server{}
			for _, dialect := range []struct {
				name string
				path string
			}{
				{"openai", "/v1/chat/completions"},
				{"anthropic", "/v1/messages"},
				{"ollama", "/api/generate"},
			} {
				t.Run(dialect.name, func(t *testing.T) {
					w := httptest.NewRecorder()
					req := routedRequest(httptest.NewRequest(http.MethodPost, dialect.path, nil))
					s.proxyToInstance(w, req, inst, true)
					assert.Equal(t, http.StatusServiceUnavailable, w.Code)
					assert.Equal(t, "5", w.Header().Get("Retry-After"))
					assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
					var body map[string]any
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
					if dialect.name == "ollama" {
						assert.Equal(t, reason.code, body["code"])
						assert.NotEmpty(t, body["error"])
					} else {
						failure, ok := body["error"].(map[string]any)
						require.True(t, ok)
						assert.Equal(t, reason.code, failure["code"])
						assert.Equal(t, "api_error", failure["type"])
					}
				})
			}
			t.Run("in_band", func(t *testing.T) {
				w := httptest.NewRecorder()
				openEventStream(w)
				req := routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
				streamer := &admissionStreamRecorder{InBandStreamer: openaiproto.New()}
				s.streamFromInstance(w, req, inst, streamer)
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Empty(t, w.Result().Header.Get("Retry-After"))
				assert.Equal(t, http.StatusServiceUnavailable, streamer.failure.Status)
				assert.Equal(t, instance.AdmissionRetryAfter, streamer.failure.RetryAfter)
				assert.Equal(t, reason.code, streamer.failure.Code)
				assert.Contains(t, w.Body.String(), `"code":"`+reason.code+`"`)
				assert.Contains(t, w.Body.String(), `"type":"api_error"`)
			})
			assert.EqualValues(t, 1, inst.GetActiveRequests())
		})
	}
}
