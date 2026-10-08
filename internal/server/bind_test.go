package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bindTarget struct {
	ID   string `json:"id" binding:"required"`
	RPM  int    `json:"rpm_limit,omitempty"`
	Note string `json:"note,omitempty"`
}

func postJSON(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/keys", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func TestBindJSONStrict(t *testing.T) {
	// Field names in validator errors come from the tag translator the
	// server registers at startup; strict binding runs the same validator.
	RegisterValidatorTranslator()
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		optional bool
		wantOK   bool
		wantCode int
		wantIn   string
	}{
		{
			name:   "known fields bind",
			body:   `{"id":"k1","rpm_limit":30}`,
			wantOK: true,
		},
		{
			// The reported defect: a key "scoped" with a field keys do not
			// carry used to come back 201 with the field dropped.
			name:     "unknown field is a 400 that names it",
			body:     `{"id":"k1","allowed_models":["a"]}`,
			wantCode: http.StatusBadRequest,
			wantIn:   `unknown field \"allowed_models\"`,
		},
		{
			name:     "validation still reports the missing field",
			body:     `{"rpm_limit":30}`,
			wantCode: http.StatusBadRequest,
			wantIn:   "id is required",
		},
		{
			name:     "wrong type is reported as a type error",
			body:     `{"id":"k1","rpm_limit":"thirty"}`,
			wantCode: http.StatusBadRequest,
			wantIn:   "rpm_limit",
		},
		{
			name:     "empty body is rejected when required",
			body:     ``,
			wantCode: http.StatusBadRequest,
			wantIn:   "request body is required",
		},
		{
			name:     "empty body is accepted when optional",
			body:     ``,
			optional: true,
			wantOK:   true,
		},
		{
			name:     "unknown field is still rejected when the body is optional",
			body:     `{"nope":1}`,
			optional: true,
			wantCode: http.StatusBadRequest,
			wantIn:   `unknown field \"nope\"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, rec := postJSON(t, tt.body)

			var req bindTarget
			var ok bool
			if tt.optional {
				ok = BindJSONStrictOptional(c, &req)
			} else {
				ok = BindJSONStrict(c, &req)
			}

			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				return
			}
			require.Equal(t, tt.wantCode, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.wantIn)
		})
	}
}

// The compat and cluster-internal surfaces must keep ignoring fields they
// do not model, so the lenient binder stays lenient.
func TestBindJSON_ToleratesUnknownFields(t *testing.T) {
	t.Parallel()
	c, _ := postJSON(t, `{"id":"k1","future_field":true}`)

	var req bindTarget
	require.True(t, BindJSON(c, &req))
	assert.Equal(t, "k1", req.ID)
}
