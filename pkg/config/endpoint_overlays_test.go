package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLoadRefusesUnknownOverlayNames(t *testing.T) {
	for _, leaf := range []struct {
		name  string
		value map[string]any
	}{
		{"parameters", map[string]any{"parameters": map[string]any{"threads": "2"}}},
		{"environment", map[string]any{"environment": map[string]any{"THREADS": "2"}}},
		{"empty", map[string]any{}},
	} {
		for _, tier := range []string{"defaults", "models.m", "nodes.n", "nodes.n.models.m"} {
			t.Run("llamacpp/"+tier+"/"+leaf.name, func(t *testing.T) {
				dir := t.TempDir()
				require.NoError(t, templates.InstallDefaults(dir))
				path := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, yaml.Unmarshal(data, &body))
				cell := map[string]any{"endpoints": map[string]any{"embedding": leaf.value}}
				switch tier {
				case "defaults":
					body["defaults"].(map[string]any)["endpoints"] = cell["endpoints"]
				case "models.m":
					body["models"] = map[string]any{"m": cell}
				case "nodes.n":
					body["nodes"] = map[string]any{"n": cell}
				case "nodes.n.models.m":
					body["nodes"] = map[string]any{"n": map[string]any{"models": map[string]any{"m": cell}}}
				}
				data, err = yaml.Marshal(body)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o600))
				_, err = LoadAppsConfig(dir)
				var problems TreeProblems
				require.ErrorAs(t, err, &problems)
				require.Len(t, problems, 1)
				assert.Equal(t, tier+".endpoints.embedding", problems[0].Path)
				assert.Equal(t, "chat, embeddings, reranking", problems[0].Want)
				assert.NotErrorIs(t, err, ErrInvalidVariant)
			})
		}
	}
}

func TestExternalValidationRefusesUnknownOverlayNames(t *testing.T) {
	for _, overlay := range []EndpointOverlay{{Parameters: map[string]string{"threads": "2"}}, {Environment: map[string]string{"THREADS": "2"}}, {}} {
		eps := map[string]EndpointOverlay{"embedding": overlay}
		p := ExternalProvider{Name: "external", Protocol: ProtocolOpenAI,
			Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
			Runtime:      ExternalRuntime{Endpoint: "http://localhost:1234"},
			Defaults:     &AppDefaultsConfig{Endpoints: eps}, Models: map[string]ModelSpec{"m": {Endpoints: eps}},
			Nodes: map[string]NodeSpec{"n": {Endpoints: eps, Models: map[string]NodeModelSpec{"m": {Endpoints: eps}}}},
		}
		err := p.Validate()
		var problems TreeProblems
		require.ErrorAs(t, err, &problems)
		require.Len(t, problems, 4)
		var paths []string
		for _, problem := range problems {
			paths = append(paths, problem.Path)
		}
		assert.ElementsMatch(t, []string{"defaults.endpoints.embedding", "models.m.endpoints.embedding", "nodes.n.endpoints.embedding", "nodes.n.models.m.endpoints.embedding"}, paths)
		assert.NotErrorIs(t, err, ErrInvalidVariant)
	}
}
