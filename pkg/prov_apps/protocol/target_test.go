package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
)

// recordingEndpoint answers every request a protocol client makes and
// records the path and Authorization header of each.
type recordingEndpoint struct {
	mu   sync.Mutex
	seen map[string]string // path -> Authorization
}

func newRecordingEndpoint(t *testing.T) (*recordingEndpoint, *httptest.Server) {
	rec := &recordingEndpoint{seen: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen[r.URL.Path] = r.Header.Get("Authorization")
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"m"}],"data":[{"id":"m"}],"version":"1"}`))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func bearer(token string) backend.Upstream {
	return backend.Credential(&config.APIConfig{AuthType: config.AuthTypeBearer, Token: token})
}

// A provider that declares a credential gets it on every request zzRouter
// makes to it, the connection probe included; one that declares none, or
// whose caller key would travel on a forward, gets nothing, since a call
// zzRouter makes on its own has no caller.
func TestProtocolCallsCarryTheTargetsCredential(t *testing.T) {
	cases := []struct {
		name string
		up   backend.Upstream
		want string
	}{
		{"bearer", bearer("provider-token"), "Bearer provider-token"},
		{"none", backend.Engine(), ""},
		{"caller", backend.CallerKey(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			rec, srv := newRecordingEndpoint(t)
			target := NewTarget(srv.URL, tc.up)
			ollama := NewOllamaProvider(nil)
			_, err := ollama.ListModels(ctx, target)
			require.NoError(t, err)
			_, _ = ollama.ListRunningModels(ctx, target)
			_ = ollama.LoadModel(ctx, target, "m", nil)
			_ = ollama.UnloadModel(ctx, target, "m")
			_ = ollama.DeleteModel(ctx, target, "m")
			_, _ = ollama.ShowApp(ctx, target)
			for _, path := range []string{"/v1/models", "/api/tags", "/api/ps", "/api/generate", "/api/delete", "/api/version"} {
				assert.Equal(t, tc.want, rec.seen[path], "ollama %s", path)
			}

			rec, srv = newRecordingEndpoint(t)
			_, err = NewOpenAIProvider("vllm", 0, "openai", nil).ListModels(ctx, NewTarget(srv.URL, tc.up))
			require.NoError(t, err)
			assert.Equal(t, tc.want, rec.seen["/v1/models"], "openai /v1/models")
		})
	}
}

// A provider holds no credential of its own, so an edited one applies on
// the next call, without a restart.
func TestProtocolCredentialFollowsTheTarget(t *testing.T) {
	rec, srv := newRecordingEndpoint(t)
	p := NewOpenAIProvider("ext", 0, "openai", nil)

	_, err := p.ListModels(context.Background(), NewTarget(srv.URL, bearer("old")))
	require.NoError(t, err)
	_, err = p.ListModels(context.Background(), NewTarget(srv.URL, bearer("new")))
	require.NoError(t, err)
	assert.Equal(t, "Bearer new", rec.seen["/v1/models"])
}

// An endpoint behind a path prefix keeps it; a trailing /v1 is not part
// of the base, so it is not doubled.
func TestTargetBase(t *testing.T) {
	assert.Equal(t, "https://openrouter.ai/api", NewTarget("https://openrouter.ai/api/v1/", backend.Engine()).base)
	assert.Equal(t, "http://nas.lan:11434", NewTarget("http://nas.lan:11434", backend.Engine()).base)
	assert.Equal(t, "nas.lan:11434", NewTarget("http://nas.lan:11434/", backend.Engine()).Host())
}

// A hosted API is called directly: its own answer to a bad key is what
// the caller sees, not a connection probe's "failed to connect".
func TestCloudTargetIsNotProbed(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	cloud := TargetOf(&backend.Resolved{Endpoint: srv.URL, Upstream: bearer("bad"), Cloud: true})
	_, err := NewOpenAIProvider("openrouter", 0, "openai", nil).ListModels(context.Background(), cloud)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), "failed to connect")
	assert.Len(t, paths, 1, "one request: the call itself")
}
