package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// ctxWithHeaders builds a gin context carrying the given headers.
func ctxWithHeaders(t *testing.T, headers map[string]string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodGet, "/zzrouter/v1/models", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c
}

func TestReadCredential(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		wantKey    string
		wantScheme string
	}{
		{
			name:       "x-api-key",
			headers:    map[string]string{"X-API-Key": "key-alpha"},
			wantKey:    "key-alpha",
			wantScheme: "X-API-Key",
		},
		{
			name:       "authorization bearer",
			headers:    map[string]string{"Authorization": "Bearer key-beta"},
			wantKey:    "key-beta",
			wantScheme: "Authorization: Bearer",
		},
		{
			name:       "bearer scheme is case-insensitive",
			headers:    map[string]string{"Authorization": "bearer key-gamma"},
			wantKey:    "key-gamma",
			wantScheme: "Authorization: Bearer",
		},
		{
			name: "explicit header wins over bearer",
			headers: map[string]string{
				"X-API-Key":     "key-explicit",
				"Authorization": "Bearer key-bearer",
			},
			wantKey:    "key-explicit",
			wantScheme: "X-API-Key",
		},
		{
			name:    "non-bearer scheme is not mistaken for a key",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			wantKey: "",
		},
		{
			name:    "bearer with no token",
			headers: map[string]string{"Authorization": "Bearer "},
			wantKey: "",
		},
		{
			name:    "bearer with no separator",
			headers: map[string]string{"Authorization": "Bearer"},
			wantKey: "",
		},
		{
			name:    "no credential",
			headers: map[string]string{},
			wantKey: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred := readCredential(ctxWithHeaders(t, tt.headers), nodeAPIKeyHeaders)
			assert.Equal(t, tt.wantKey, cred.key)
			assert.Equal(t, tt.wantKey != "", cred.found())
			if tt.wantScheme != "" {
				assert.Equal(t, tt.wantScheme, cred.scheme)
			}
		})
	}
}

func TestMissingCredentialDetail(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "absent credential names both accepted schemes",
			headers: map[string]string{},
			want:    "API key required: send it as \"Authorization: Bearer <key>\" or in the X-API-Key header",
		},
		{
			name:    "unsupported scheme is named back to the caller",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			want:    "Unsupported Authorization scheme \"Basic\": use \"Authorization: Bearer <key>\" or the X-API-Key header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, missingCredentialDetail(ctxWithHeaders(t, tt.headers)))
		})
	}
}

// TestAcceptedAPIKeyHeaders_ClusterPrecedence pins the ordering that
// coordinator-to-worker dispatch depends on: a worker must resolve
// X-Cluster-API-Key ahead of anything else, and Authorization must
// stay last on both surfaces so adding it can never displace a header
// that already authenticated.
func TestAcceptedAPIKeyHeaders_ClusterPrecedence(t *testing.T) {
	worker := &pkgConfig.NodeConfig{}
	worker.Cluster.Mode = "worker"
	require.True(t, worker.Cluster.IsWorker(), "fixture must be a worker")

	got := acceptedAPIKeyHeaders(ctxWithHeaders(t, nil), worker)
	require.NotEmpty(t, got)
	assert.Equal(t, "X-Cluster-API-Key", got[0], "coordinator dispatch header must win on a worker")
	assert.Equal(t, authorizationHeader, got[len(got)-1], "bearer must remain the last resort")

	node := acceptedAPIKeyHeaders(ctxWithHeaders(t, nil), &pkgConfig.NodeConfig{})
	assert.Equal(t, "X-API-Key", node[0])
	assert.Equal(t, authorizationHeader, node[len(node)-1], "bearer must remain the last resort")
}

// TestReadCredential_PerSurfacePrecedence pins the one place the two
// surfaces genuinely disagree. The compatibility surface resolves
// Authorization first and always has: a client sending a bearer token
// and a stale X-API-Key authenticates on the bearer there, and on the
// X-API-Key everywhere else. Flipping either silently changes which
// credential wins for a client that sends both.
func TestReadCredential_PerSurfacePrecedence(t *testing.T) {
	both := map[string]string{
		"X-API-Key":     "key-header",
		"Authorization": "Bearer key-bearer",
	}

	compat := readCredential(ctxWithHeaders(t, both), compatAPIKeyHeaders)
	if compat.key != "key-bearer" {
		t.Errorf("compat surface: got %q, want the bearer token", compat.key)
	}

	management := readCredential(ctxWithHeaders(t, both), nodeAPIKeyHeaders)
	if management.key != "key-header" {
		t.Errorf("management surface: got %q, want the X-API-Key", management.key)
	}

	// A worker resolves the coordinator's dispatch header ahead of both.
	clusterHeaders := map[string]string{
		"X-Cluster-API-Key": "key-cluster",
		"X-API-Key":         "key-header",
		"Authorization":     "Bearer key-bearer",
	}
	cluster := readCredential(ctxWithHeaders(t, clusterHeaders), clusterAPIKeyHeaders)
	if cluster.key != "key-cluster" {
		t.Errorf("cluster context: got %q, want the coordinator dispatch key", cluster.key)
	}
}

// TestAllResponders_SetAuthenticateChallenge pins the contract stated on
// httperr.Responder.Unauthorized. The three dialects write 401s through
// different code, so nothing but a test keeps them agreeing that a 401
// carries the RFC 7235 challenge.
func TestAllResponders_SetAuthenticateChallenge(t *testing.T) {
	responders := newResponderSet()
	cases := map[string]httperr.Responder{
		"problem": responders.problem,
		"ollama":  responders.ollama,
		"openai":  responders.openai,
	}

	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/anything", nil)

			r.Unauthorized(c, "nope")

			if got := w.Header().Get("WWW-Authenticate"); got != httperr.AuthenticateChallenge {
				t.Errorf("%s responder: WWW-Authenticate = %q, want %q",
					name, got, httperr.AuthenticateChallenge)
			}
		})
	}
}
