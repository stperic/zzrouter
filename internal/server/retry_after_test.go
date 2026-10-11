package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol/anthropic"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
)

// Every dialect's writer-level WriteError renders Error.RetryAfter the same way.
func TestWriteErrorRetryAfterAcrossDialects(t *testing.T) {
	dialects := []struct {
		name      string
		responder httperr.Responder
	}{
		{"problem", newProblemResponder()},
		{"ollama", newOllamaResponder()},
		{"anthropic", anthropic.New(nil)},
		{"openai", openaiproto.New()},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			t.Run("sets Retry-After from RetryAfter", func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				d.responder.WriteError(rec, req, httperr.Error{
					Status: 503, Type: "server_error", Code: "x", Message: "m",
					RetryAfter: 1500 * time.Millisecond,
				})
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
				assert.Equal(t, "2", rec.Header().Get("Retry-After"))
			})
			t.Run("omits Retry-After when RetryAfter is zero", func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				d.responder.WriteError(rec, req, httperr.Error{
					Status: 503, Type: "server_error", Code: "x", Message: "m",
				})
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
				assert.Empty(t, rec.Header().Values("Retry-After"))
			})
		})
	}
}
