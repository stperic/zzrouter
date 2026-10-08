package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOllamaCatalogVisionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, show    string
		known, vision bool
	}{{"vision", `{"capabilities":["completion","vision"]}`, true, true}, {"text", `{"capabilities":["completion"]}`, true, false}, {"empty", `{"capabilities":[]}`, true, false}, {"older", `{}`, false, false}, {"broken", `not json`, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer daemon-token", r.Header.Get("Authorization"))
				if r.URL.Path == "/api/tags" {
					_, _ = w.Write([]byte(`{"models":[{"name":"neutral-name","details":{"format":"gguf"}}]}`))
					return
				}
				assert.Equal(t, "/api/show", r.URL.Path)
				_, _ = w.Write([]byte(tc.show))
			}))
			t.Cleanup(daemon.Close)
			cfg := &config.AppsConfig{}
			p := config.NewOllamaConnectProvider(constants.AppOllama, daemon.URL, "daemon-token")
			require.NoError(t, cfg.AddApp(constants.AppOllama, config.ServiceConfig{Enabled: p.Enabled, Name: constants.AppOllama, Mode: constants.AppModeExternal, Protocol: p.Protocol, Runtime: &config.AppRuntimeConfig{Endpoint: daemon.URL, API: p.Runtime.API}, Capabilities: p.Capabilities}))
			c := NewConnector()
			c.ReloadConfig(cfg)
			models, err := c.ListModels(context.Background())
			require.NoError(t, err)
			require.Len(t, models, 1)
			details := models[0].Extra["details"].(map[string]any)
			vision, known := details["vision"].(bool)
			assert.Equal(t, tc.known, known)
			assert.Equal(t, tc.vision, vision)
		})
	}
}
