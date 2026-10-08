package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// patchProviderBody runs one PATCH body through the controller's request
// handling. The service is nil on purpose: every case here must be
// rejected before the controller reaches it, so reaching it at all would
// panic and fail the test loudly.
func patchProviderBody(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctrl := &ProvidersController{}
	r := gin.New()
	r.PATCH("/zzrouter/v1/providers/:name", ctrl.UpdateProvider)

	req := httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/providers/ollama",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A PATCH carrying nothing this endpoint implements used to answer 200
// with a zero-valued body — name "", enabled false — for a provider that
// was neither. `null` is the trap: into a *string it is
// indistinguishable from absent, so a caller borrowing the merge-patch
// convention from PATCH /providers/:name/parameters (where null deletes
// a key) got silence dressed up as success.
func TestUpdateProviderRejectsNothingActionable(t *testing.T) {
	cases := map[string]string{
		"null pin":     `{"pinned_version":null}`,
		"empty object": `{}`,
		"both null":    `{"enabled":null,"pinned_version":null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := patchProviderBody(t, body)

			require.Equal(t, http.StatusBadRequest, w.Code,
				"body=%s must not report success while changing nothing", body)

			var problem map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
			assert.Contains(t, problem["detail"], "pinned_version",
				"the error has to say which field would have worked")
		})
	}
}

// Clearing a pin is "", and that must stay distinct from "no field
// sent" — otherwise there is no way to unpin at all.
func TestUpdateProviderEmptyStringClearsPinIsActionable(t *testing.T) {
	// Reaching the nil service means the guard let it through, which is
	// what this asserts; the panic is recovered so the test reports it
	// as a pass rather than a crash.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal(`{"pinned_version":""} must be treated as an actionable update`)
		}
	}()
	_ = patchProviderBody(t, `{"pinned_version":""}`)
}
