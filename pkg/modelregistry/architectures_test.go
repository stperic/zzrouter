package modelregistry

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stretchr/testify/require"
)

func architectureConfig(t *testing.T) *config.AppsConfig {
	t.Helper()
	cfg := &config.AppsConfig{}
	for name, architectures := range map[string][]string{"llamacpp": nil, "strata": {"qwen4exp"}} {
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{Mode: "on-demand", Enabled: new(true), Protocol: config.ProtocolOpenAI,
			Runtime:      &config.AppRuntimeConfig{BasePort: 8100, PortRange: []int{8100, 8109}, Execution: config.ExecutionConfig{Type: "cli", Command: "test-engine"}},
			Capabilities: &config.AppCapabilities{Formats: []string{"gguf"}, Architectures: architectures, Priority: 5, WireEndpoints: []string{"chat_completions"}}}))
	}
	return cfg
}

func TestArchitectureShadowing(t *testing.T) {
	cfg := architectureConfig(t)
	for architecture, want := range map[string]string{"qwen4exp": "strata", "qwen3": "llamacpp", "": "llamacpp"} {
		provider, _ := AutoAssignProvider("model", "gguf", architecture, cfg)
		require.Equal(t, want, provider)
	}
	require.NoError(t, cfg.UpdateApp("llamacpp", func(svc *config.ServiceConfig) error {
		svc.Capabilities.Models = []string{"explicit*"}
		return nil
	}))
	provider, _ := AutoAssignProvider("explicit-model", "gguf", "qwen4exp", cfg)
	require.Equal(t, "llamacpp", provider, "operator's explicit model match keeps winning")
}

func architectureGGUF(t *testing.T, architecture string) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, v := range []any{uint32(0x46554747), uint32(3), uint64(0), uint64(2), uint64(len("general.architecture"))} {
		require.NoError(t, binary.Write(&b, binary.LittleEndian, v))
	}
	b.WriteString("general.architecture")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(8)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint64(len(architecture))))
	b.WriteString(architecture)
	return b.Bytes() // The second entry is truncated: assignment must never parse it.
}

func TestArchitectureHeaderWins(t *testing.T) {
	for _, architecture := range []string{"qwen4exp", "qwen3", "future-family"} {
		t.Run(architecture, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "org", "model-GGUF")
			require.NoError(t, os.MkdirAll(repo, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(repo, "config.json"), []byte(`{"model_type":"qwen4exp"}`), 0600))
			first := filepath.Join(repo, "model-00001-of-00002.gguf")
			require.NoError(t, os.WriteFile(first, architectureGGUF(t, architecture), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(repo, "model-00002-of-00002.gguf"), []byte("GGUF: no architecture in shard 2"), 0600))
			models, err := huggingface.NewConnector(root).ScanModels()
			require.NoError(t, err)
			require.Len(t, models, 1)
			require.Equal(t, first, models[0].FullPath)
			cfg := architectureConfig(t)
			r := &Registry{appsConfigFn: func() *config.AppsConfig { return cfg }}
			r.autoAssignApps(models)
			details := models[0].ToOllamaFormat("node")["details"].(map[string]any)
			require.Equal(t, architecture, details["family"])
			want := "llamacpp"
			if architecture == "qwen4exp" {
				want = "strata"
			}
			require.Equal(t, want, models[0].AssignedApp)
		})
	}
}

func TestArchitectureHeadersStayWithTheirVariant(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "org", "mixed-GGUF")
	require.NoError(t, os.MkdirAll(repo, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "config.json"), []byte(`{"model_type":"hf-family"}`), 0600))
	for name, body := range map[string][]byte{
		"a.gguf": architectureGGUF(t, "qwen4exp"),
		"b.gguf": architectureGGUF(t, "qwen3"),
		"c.gguf": []byte("no header: retain Hugging Face evidence"),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), body, 0600))
	}
	models, err := huggingface.NewConnector(root).ScanModels()
	require.NoError(t, err)
	require.Len(t, models, 3)
	cfg := architectureConfig(t)
	r := &Registry{appsConfigFn: func() *config.AppsConfig { return cfg }}
	r.autoAssignApps(models)
	for _, model := range models {
		want := map[string]string{"a.gguf": "qwen4exp", "b.gguf": "qwen3", "c.gguf": "hf-family"}[filepath.Base(model.FullPath)]
		details := model.ToOllamaFormat("node")["details"].(map[string]any)
		require.Equal(t, want, details["family"], model.Name)
		require.Equal(t, []string{want}, details["families"], model.Name)
	}
}
