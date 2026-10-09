package anthropic

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/assert"
)

func TestEndStream_PerDialect(t *testing.T) {
	rec := httptest.NewRecorder()
	New(nil).EndStream(rec, httperr.Error{Status: http.StatusTooManyRequests, Type: "api_error", Message: "cut"})
	assert.Equal(t, "\nevent: error\ndata: {\"error\":{\"message\":\"cut\",\"type\":\"rate_limit_error\"},\"type\":\"error\"}\n\n", rec.Body.String())
	assert.True(t, rec.Flushed)
	assert.NotContains(t, rec.Body.String(), "message_stop")
}
