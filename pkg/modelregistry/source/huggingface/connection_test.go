package huggingface

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/require"
)

func TestManagedHFConnectionChangesListingDetailsAndDownload(t *testing.T) {
	t.Setenv("HF_TOKEN", "external-hf-fixture")
	var mu sync.Mutex
	var headers []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path == "/org/repo/resolve/main/model.safetensors" {
			_, _ = io.WriteString(w, "weights")
			return
		}
		if r.URL.RawQuery == "blobs=true" {
			_, _ = io.WriteString(w, `{"siblings":[{"rfilename":"model.safetensors","size":7}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"pipeline_tag":"text-to-image"}`)
	}))
	defer upstream.Close()
	cfg := &config.AppsConfig{}
	//nolint:gosec // G101: synthetic credential placeholders exercise declared auth, never real keys.
	require.NoError(t, cfg.AddApp("huggingface", config.ServiceConfig{Mode: "registry", Enabled: new(true), Connection: &config.ConnectionConfig{Fields: []config.ConnectionField{{ID: "token", Label: "Token", Type: "text", Env: "HF_TOKEN", Secret: true}}}, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "${HF_TOKEN}"}}}))
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "connections.enc"), bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	defer func() { _ = vault.Close() }()
	resolver := backend.NewResolver(func() *config.AppsConfig { return cfg }, vault)
	connector := NewConnector(t.TempDir(), resolver)
	for revision, token := range []string{"first-hf-fixture", "second-hf-fixture", ""} {
		if token != "" {
			_, err = vault.Replace(t.Context(), "huggingface", uint64(revision), map[string]string{"token": token, backend.ConnectionEndpointField: upstream.URL})
		} else {
			_, err = vault.Remove(t.Context(), "huggingface", uint64(revision))
		}
		require.NoError(t, err)
		mu.Lock()
		headers = nil
		mu.Unlock()
		exists, err := connector.ModelExists(t.Context(), "org/repo")
		require.NoError(t, err)
		require.True(t, exists)
		_, err = connector.ShowModel(t.Context(), "org/repo")
		require.NoError(t, err)
		_, err = connector.GetRepoFiles(t.Context(), "org/repo")
		require.NoError(t, err)
		request, err := ParseSource("org/repo#model.safetensors")
		require.NoError(t, err)
		require.NoError(t, connector.Download(t.Context(), request, nil, targetDestination{dir: t.TempDir(), name: "weights.safetensors"}))
		mu.Lock()
		observed := append([]string(nil), headers...)
		mu.Unlock()
		require.GreaterOrEqual(t, len(observed), 5)
		want := ""
		if token != "" {
			want = "Bearer " + token
		}
		for _, header := range observed {
			require.Equal(t, want, header, "every source operation resolves current credentials; removal retains public access")
		}
	}
}

func TestHFDownloadRedirectDoesNotCarryCredentialToAnotherOrigin(t *testing.T) {
	received := make(chan string, 1)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "weights")
	}))
	defer cdn.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound) }))
	defer hub.Close()
	connector := &Connector{token: "private-hf-fixture"} //nolint:gosec // G101: synthetic token verifies redirect scrubbing.
	request, err := connector.newGet(t.Context(), hub.URL)
	require.NoError(t, err)
	response, err := connector.httpClient(httpTimeout).Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, "", <-received)
}

func TestHFRedirectStripsAllProviderHeadersAndPreservesRange(t *testing.T) {
	t.Setenv("HF_CUSTOM_FIXTURE", "custom-fixture-secret")
	received := make(chan http.Header, 1)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		_, _ = io.WriteString(w, "weights")
	}))
	defer cdn.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "custom-fixture-secret", r.Header.Get("X-Private-Token"))
		require.Equal(t, "custom-fixture-secret", r.Header.Get("X-Additional-Credential"))
		http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound)
	}))
	defer hub.Close()
	cfg := &config.AppsConfig{}
	//nolint:gosec // G101: synthetic credential placeholders exercise declared auth, never real keys.
	require.NoError(t, cfg.AddApp("huggingface", config.ServiceConfig{Mode: "registry", Connection: &config.ConnectionConfig{Fields: []config.ConnectionField{{ID: "token", Label: "Token", Type: "text", Env: "HF_CUSTOM_FIXTURE", Secret: true}}}, Runtime: &config.AppRuntimeConfig{Endpoint: hub.URL, API: &config.APIConfig{AuthType: config.AuthTypeCustom, AuthHeader: "X-Private-Token", Token: "${HF_CUSTOM_FIXTURE}", CustomHeaders: map[string]string{"X-Additional-Credential": "${HF_CUSTOM_FIXTURE}"}}}}))
	connector := NewConnector(t.TempDir(), backend.NewResolver(func() *config.AppsConfig { return cfg }))
	request, err := connector.newGet(t.Context(), huggingFaceDownloadBase+"org/repo/resolve/main/model.bin")
	require.NoError(t, err)
	request.Header.Set("Range", "bytes=100-")
	response, err := connector.httpClient(httpTimeout).Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	observed := <-received
	require.Empty(t, observed.Get("X-Private-Token"))
	require.Empty(t, observed.Get("X-Additional-Credential"))
	require.Equal(t, "bytes=100-", observed.Get("Range"))
}
