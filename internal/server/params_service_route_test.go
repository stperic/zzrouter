package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The public service routes used to be mounted on the Internal handlers,
// which answer about the node they run on by design. ?node= was accepted,
// ignored, and answered locally: asking a macOS coordinator for a Debian
// worker's ollama returned the coordinator's own Homebrew pid, with
// nothing in the body to say which node had replied.
func TestServiceRoutesHonourNodeQuery(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	srv := httptest.NewServer(s.engine)
	defer closeHTTPTestServer(srv)

	call := func(t *testing.T, method, url string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("X-API-Key", TestAdminKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	// An unnamed node still answers here, so the two cases have to be
	// distinguishable by more than "did it succeed".
	for _, tc := range []struct {
		name, method, path string
	}{
		{"status", http.MethodGet, "/zzrouter/v1/providers/ollama/service/status"},
		{"apply", http.MethodPost, "/zzrouter/v1/providers/ollama/service/apply"},
		{"start", http.MethodPost, "/zzrouter/v1/providers/ollama/service/start"},
		{"stop", http.MethodPost, "/zzrouter/v1/providers/ollama/service/stop"},
		{"restart", http.MethodPost, "/zzrouter/v1/providers/ollama/service/restart"},
	} {
		t.Run(tc.name+" routes to the node named", func(t *testing.T) {
			code, body := call(t, tc.method, srv.URL+tc.path+"?node=no-such-worker")

			assert.Equal(t, http.StatusBadGateway, code,
				"an unreachable node must fail; answering about the local one is the bug")
			assert.Contains(t, body["detail"], "no-such-worker",
				"the error should name the node the caller asked for")
		})
	}
}

// The split is the fix, so pin it: internal handlers answer for their own
// node and must never be what the public surface mounts. Asserting on the
// handler name follows isRetiredRoute, which identifies routes the same way.
func TestServiceRoutesMountTheRoutingHandler(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	got := map[string]string{}
	for _, r := range s.engine.Routes() {
		if strings.Contains(r.Path, "/service/") {
			got[r.Method+" "+r.Path] = r.Handler
		}
	}

	for route, want := range map[string]string{
		"GET /zzrouter/v1/providers/:name/service/status":          "HandleServiceStatus-fm",
		"POST /zzrouter/v1/providers/:name/service/apply":          "HandleApplyService-fm",
		"GET /zzrouter/v1/internal/providers/:name/service/status": "HandleInternalGetServiceStatus-fm",
		"POST /zzrouter/v1/internal/providers/:name/service/apply": "HandleInternalApplyService-fm",
	} {
		handler, mounted := got[route]
		require.True(t, mounted, "%s is not registered", route)
		assert.True(t, strings.HasSuffix(handler, want),
			"%s should be served by %s, got %s", route, want, handler)
	}
}

func TestServiceControlsRequireAdmin(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, action := range []string{"start", "stop", "restart", "apply"} {
		for _, key := range []string{"", TestUserKey} {
			request := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/providers/ollama/service/"+action, nil)
			request.Header.Set("X-API-Key", key)
			response := httptest.NewRecorder()
			s.engine.ServeHTTP(response, request)
			require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, response.Code, "%s cannot authorize %s", key, action)
		}
	}
}
