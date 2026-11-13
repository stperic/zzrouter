package search

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	tea "charm.land/bubbletea/v2"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func TestDirectIntentAfterReentryDeploysOnlyCurrentModel(t *testing.T) {
	for _, oldCloud := range []bool{false, true} {
		for _, newCloud := range []bool{false, true} {
			t.Run(fmt.Sprintf("old-cloud=%t/new-cloud=%t", oldCloud, newCloud), func(t *testing.T) {
				payloads := make(chan map[string]any, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var data any
					switch r.URL.Path {
					case apipath.RegistriesOllamaTags:
						data = metadata.OllamaTags{Tags: []metadata.OllamaTag{{Name: "A-selected", IsCloud: oldCloud}, {Name: "A-other"}}}
					case apipath.ModelCard("ollama", "model-A"):
						data = metadata.DiscoveryResult{Discovery: metadata.Discovery{Provider: "ollama", APIID: "model-A", Description: "A details"}}
					case apipath.ModelCard("ollama", "model-B"):
						data = metadata.DiscoveryResult{Discovery: metadata.Discovery{Provider: "ollama", APIID: "model-B", IsCloud: newCloud}}
					case apipath.Nodes:
						data = []client.NodeWithProviders{{Name: "worker"}}
					case apipath.NodesCompatible:
						data = []any{}
					case apipath.Deployments:
						require.Equal(t, http.MethodPost, r.Method)
						var payload map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
						payloads <- payload
						data = client.Deployment{ID: "B-deployment"}
					default:
						t.Errorf("unexpected request: %s", r.URL)
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				t.Cleanup(server.Close)
				m := activeSearch(t, &discoveryFixture{})
				m.client = client.NewClient(config.ClientNodeConfig{Address: server.URL})
				m.Provider = "ollama"
				m.results = []searchResult{{Provider: "ollama", Name: "model-A"}}
				_, variants := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				require.NotNil(t, variants)
				m.Update(variants())
				require.Equal(t, stateVariantSelect, m.State)
				_, detail := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				require.NotNil(t, detail)
				oldDetail := detail()
				m.Update(oldDetail)
				require.Equal(t, "A-selected", m.selectedVariant.Filename)
				m.Deactivate()
				m.State = StateList
				_ = m.Activate()
				m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
				require.Equal(t, stateDirectPull, m.State)
				m.directPullInput = "model-B"
				_, validate := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				require.NotNil(t, validate)
				m.Update(oldDetail)
				require.Equal(t, stateDirectPull, m.State, "queued A work cannot take over the new intent")
				_, next := m.Update(validate())
				require.NotNil(t, next)
				if !newCloud {
					require.Equal(t, StateDetail, m.State, "a local B must not inherit A's cloud classification")
					m.Update(next())
					require.Equal(t, stateNodeSelect, m.State)
					m.Update(tea.KeyPressMsg{Code: ' '})
					_, next = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				}
				require.NotNil(t, next)
				require.True(t, m.NavigationBlocked())
				m.Update(next())
				payload := <-payloads
				require.Equal(t, "model-B", payload["model"])
				require.Equal(t, "ollama", payload["registry"])
				if newCloud {
					require.Equal(t, "cloud", payload["file"], "only B's validated cloud tag may select a file")
					require.NotContains(t, payload, "nodes")
				} else {
					require.NotContains(t, payload, "file", "a new direct intent cannot reuse A's selected file")
					require.Equal(t, []any{"worker"}, payload["nodes"])
				}
				require.False(t, m.NavigationBlocked())
				require.Equal(t, "B-deployment", m.LastDeployment.ID)
				require.Empty(t, m.variants, "a new intent cannot retain A's variant picker")
			})
		}
	}
}
