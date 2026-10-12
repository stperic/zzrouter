package modelregistry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssignmentPublicationContractUnknownFormat(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("gateway", config.ServiceConfig{Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "http://synthetic.invalid"}, Capabilities: &config.AppCapabilities{Models: []string{"explicit"}, Formats: []string{"gguf"}, WireEndpoints: []string{"chat_completions"}}}))
	root := t.TempDir()
	for _, name := range []string{"explicit", "model-gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name+".llm"), []byte("synthetic weights"), 0600))
	}
	r := &Registry{modelsRoot: root, huggingfaceConnector: huggingface.NewConnector(root), appsConfigFn: func() *config.AppsConfig { return cfg }}
	models, _, err := r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, models, 2)
	for _, model := range models {
		assert.Equal(t, "gateway", model.AssignedApp, model.Name)
		require.Empty(t, model.Format, "assignment inference must not invent source format evidence")
	}
	tagged := &metadata.ModelMetadata{Name: "tagged", Extra: map[string]any{"huggingface": map[string]any{"tags": []any{"gguf"}}}}
	r.autoAssignApps([]*metadata.ModelMetadata{tagged})
	require.Equal(t, "gateway", tagged.AssignedApp)
	require.Empty(t, tagged.Format)
}
