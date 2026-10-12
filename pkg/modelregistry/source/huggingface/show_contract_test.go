package huggingface

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/require"
)

func TestShowMetadataDoesNotGuessCapabilitiesContract(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "org", "model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"architectures":["VisionCausalLM"],"max_position_embeddings":8192}`), 0600))
	result, err := NewConnector(root).ShowModel(t.Context(), "org/model")
	require.NoError(t, err)
	require.NotContains(t, result, "capabilities", "architecture names do not establish serving capabilities")
	require.EqualValues(t, 8192, result["model_info"].(map[string]any)["max_position_embeddings"])
}

func TestShowMetadataReportedDefaultsAndTemplateContract(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "org", "model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	for _, fixture := range []struct{ name, body string }{{"config.json", `{}`}, {"generation_config.json", `{"temperature":0.7}`}, {"tokenizer_config.json", `{"chat_template":"reported template"}`}} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.name), []byte(fixture.body), 0600))
	}
	result, err := NewConnector(root).ShowModel(t.Context(), "org/model")
	require.NoError(t, err)
	require.Equal(t, true, result["template_present"])
	require.Equal(t, 0.7, result["parameters"].(map[string]any)["temperature"])
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tokenizer_config.json"), []byte(`{}`), 0600))
	result, err = NewConnector(root).ShowModel(t.Context(), "org/model")
	require.NoError(t, err)
	require.NotContains(t, result, "template_present", "missing template evidence stays missing")
}

// Synthetic API replies pin metadata error contracts without contacting HuggingFace.
func TestShowMetadataUpstreamFailureContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       int
	}{
		{"auth", `{"error":"unauthorized"}`, 401, 401},
		{"unavailable", `{"error":"unavailable"}`, 503, 503},
		{"missing", `{"error":"missing"}`, 404, 404},
		{"malformed", `{`, 200, 502},
		{"null", `null`, 200, 502},
		{"trailing", `{} {}`, 200, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()
			connector := NewConnector(t.TempDir())
			connector.apiBase = up.URL + "/"
			_, err := connector.ShowModel(t.Context(), "org/model")
			var wire httperr.Error
			require.ErrorAs(t, err, &wire)
			require.Equal(t, tc.want, wire.Status)
		})
	}
}
