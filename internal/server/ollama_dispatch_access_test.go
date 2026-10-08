package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// The Ollama surface resolved and proxied on its own, so a key restricted
// to one model served every other one through /api/chat while /v1/* denied
// it. Both surfaces run the same gate now.
func TestDispatchOllamaInference_EnforcesModelAccess(t *testing.T) {
	rig := newAccessRigWithHooks(t, ModelHooks{Identity: (&Server{}).modelIdentity})

	rig.seedTeam(t, "eng", &teams.Team{
		Name:          "Engineering",
		AllowedModels: []string{"qwen2.5:0.5b"},
	})
	raw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{Name: "Alice", Role: "user"})

	s := &Server{access: rig.access}

	body := []byte(`{"model":"smollm:135m@macbook-pro","messages":[]}`)
	c, w := ollamaRequest(t, raw, body)
	require.True(t, rig.access.Authenticate(c, RoleUser))

	err := s.dispatchOllamaInference(c, body, "smollm:135m@macbook-pro", "chat")

	// The denial is written by the gate, not returned to the handler —
	// otherwise the handler would render a second body over it.
	assert.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Model access denied")

	// Ollama's flat {"error": "..."} envelope, not the OpenAI one: the
	// gate answers in the dialect the route group attached.
	assert.Contains(t, w.Body.String(), `"error"`)
	assert.NotContains(t, w.Body.String(), `"type":"invalid_request_error"`)
}

// ollamaRequest builds a POST /api/chat context carrying the Ollama
// responder the route group installs, so denials render in that dialect.
func ollamaRequest(t *testing.T, rawKey string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	if rawKey != "" {
		req.Header.Set("X-API-Key", rawKey)
	}
	req = req.WithContext(context.Background())
	c.Request = req
	httperr.AttachResponder(newResponderSet().ollama)(c)
	return c, w
}

// writeQuotaDenial bypasses the context responder and writes the OpenAI
// envelope directly. That was invisible while /api/* skipped the
// enforcement chain; now that it runs, an Ollama client unmarshalling
// `error` as a string would fail outright on an object.
func TestDispatchOllamaInference_QuotaDenialUsesOllamaEnvelope(t *testing.T) {
	rig := newAccessRigWithHooks(t, ModelHooks{Identity: (&Server{}).modelIdentity})

	rig.seedTeam(t, "eng", &teams.Team{Name: "Engineering"})
	raw := rig.seedKey(t, "alice", "eng", string(teams.RoleOwner), &keys.VirtualKey{
		Name:        "Alice",
		Role:        "user",
		QuotaConfig: quota.QuotaConfig{RPMLimit: 1},
	})

	s := &Server{access: rig.access}
	body := []byte(`{"model":"smollm:135m","messages":[]}`)

	// First call consumes the single request allowed this minute. It gets
	// past the gate and dies at resolution (this Server has no resolver),
	// which is fine — only the second call's envelope is under test.
	first, _ := ollamaRequest(t, raw, body)
	require.True(t, rig.access.Authenticate(first, RoleUser))
	release, ok := s.enforceAndAttribute(first, "smollm:135m", nil)
	require.True(t, ok, "first request under an RPM of 1 must be allowed")
	release()

	c, w := ollamaRequest(t, raw, body)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	_, ok = s.enforceAndAttribute(c, "smollm:135m", nil)
	require.False(t, ok, "second request under an RPM of 1 must be denied")

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"), "a throttle must say when to come back")

	// Ollama's envelope is {"error": "<string>"} — decoding into a string
	// field is the exact thing an ollama client does.
	var flat struct {
		Error string `json:"error"`
	}
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &flat),
		"body is not the Ollama flat envelope: %s", w.Body.String())
	assert.NotEmpty(t, flat.Error)
}
