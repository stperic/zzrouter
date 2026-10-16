package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOllamaCatalogCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, show           string
		known, vision, tools bool
	}{
		{"both", `{"capabilities":["completion","vision","tools"]}`, true, true, true},
		{"vision", `{"capabilities":["completion","vision"]}`, true, true, false},
		{"tools", `{"capabilities":["completion","tools"]}`, true, false, true},
		{"text", `{"capabilities":["completion"]}`, true, false, false},
		{"empty", `{"capabilities":[]}`, true, false, false},
		{"older", `{}`, false, false, false},
		{"null", `{"capabilities":null}`, false, false, false},
		{"malformed list", `{"capabilities":["tools",2]}`, false, false, false},
		{"null element", `{"capabilities":["tools",null]}`, false, false, false},
		{"broken", `not json`, false, false, false},
		{"trailing malformed", `{"capabilities":["tools"]}junk`, false, false, false},
		{"HTTP failure", `{"capabilities":["tools"]}`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var shows atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer daemon-token", r.Header.Get("Authorization"))
				if r.URL.Path == "/api/tags" {
					_, _ = w.Write([]byte(`{"models":[{"name":"neutral-name","details":{"format":"gguf"}}]}`))
					return
				}
				assert.Equal(t, "/api/show", r.URL.Path)
				assert.Equal(t, http.MethodPost, r.Method)
				shows.Add(1)
				if tc.name == "HTTP failure" {
					w.WriteHeader(http.StatusBadGateway)
				}
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
			tools, known := details["tools"].(bool)
			assert.Equal(t, tc.known, known)
			assert.Equal(t, tc.tools, tools)
			assert.Equal(t, int32(1), shows.Load())
		})
	}
}
