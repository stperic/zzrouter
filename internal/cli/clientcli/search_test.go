package clientcli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func TestSearchListPrintsDeployableIdentity(t *testing.T) {
	for _, tc := range []struct {
		provider, apiID, display, want string
	}{
		{"openai", "gpt-4o", "OpenAI: Friendly GPT", "gpt-4o"},
		{"huggingface", "org/canonical-model", "Friendly repository", "org/canonical-model"},
		{"ollama", "", "registry-name-fallback", "registry-name-fallback"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == apipath.Health {
					w.WriteHeader(http.StatusOK)
					return
				}
				require.Equal(t, apipath.Search, r.URL.Path)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": metadata.SearchPage{Provider: tc.provider, Results: []metadata.DiscoveryResult{{Discovery: metadata.Discovery{Provider: tc.provider, APIID: tc.apiID, DisplayName: tc.display}}}}})
			}))
			t.Cleanup(server.Close)
			host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(t, err)
			port, err := strconv.Atoi(portText)
			require.NoError(t, err)
			cm := config.NewConfigManager("zzrouter")
			require.NoError(t, cm.SaveClientConfig(&config.ClientConfig{Node: config.NodeConnection{Address: host, Port: port}}))
			output, err := os.CreateTemp(t.TempDir(), "search-output")
			require.NoError(t, err)
			t.Cleanup(func() { _ = output.Close() })
			stdout := os.Stdout
			t.Cleanup(func() { os.Stdout = stdout })
			os.Stdout = output
			err = runSearch(t.Context(), "", tc.provider, "", "AND", 5, "")
			os.Stdout = stdout
			require.NoError(t, err)
			data, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			require.Contains(t, string(data), tc.want, "the printed identifier must be usable in a deploy request")
			if tc.apiID != "" {
				require.NotContains(t, string(data), tc.display)
			}
		})
	}
}
