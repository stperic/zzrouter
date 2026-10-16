package huggingface

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// A GGUF repo lists each weights variant once: a split set is one model
// at its first shard, and a feature's file is not a model.
func TestScanModels_ListsWeightsVariantsOnly(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "unsloth", "M-GGUF")
	for _, rel := range []string{".gitattributes", "M-Q4_K_M.gguf", "mmproj-F16.gguf",
		"Q8_0/M-Q8_0-00001-of-00002.gguf", "Q8_0/M-Q8_0-00002-of-00002.gguf"} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("GGUF"), 0o600))
	}
	require.NoError(t, integrity.RecordFiles(repo, "unsloth/M-GGUF", "", integrity.FileChecksum{RelativePath: "mmproj-F16.gguf", Size: 4, Feature: "vision"}))

	models, err := (&Connector{modelsDir: root}).ScanModels()
	require.NoError(t, err)
	got := map[string]string{}
	for _, m := range models {
		rel, _ := filepath.Rel(repo, m.FullPath)
		got[m.Name] = filepath.ToSlash(rel)
	}
	assert.Equal(t, map[string]string{
		"M-Q4_K_M": "M-Q4_K_M.gguf",
		"M-Q8_0":   "Q8_0/M-Q8_0-00001-of-00002.gguf",
	}, got)
}

func TestScanModels_IncompleteWeightsRemainVisible(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		repo     string
		files    []string
		manifest bool
	}{
		{"google/gemma-4-E2B-it", []string{".gitattributes", "README.md", "chat_template.jinja", "config.json", "generation_config.json"}, true},
		{"lmstudio-community/Qwen3.8-27B-MLX-4bit", []string{"chat_template.jinja", "config.json", "generation_config.json", "README.md"}, false},
		{"org/sharded", []string{"config.json", "model-00001-of-00002.safetensors", "model.safetensors.index.json"}, true},
		{"org/complete", []string{"config.json", "model.safetensors"}, false},
		{"org/split-GGUF", []string{".gitattributes", "m-00001-of-00002.gguf"}, false},
	}
	for _, c := range cases {
		dir := filepath.Join(root, filepath.FromSlash(c.repo))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for _, name := range c.files {
			body := "{}"
			if name == "model.safetensors.index.json" {
				body = `{"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}`
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
		}
		if c.manifest {
			manifest, err := integrity.CreateManifest(t.Context(), dir, c.repo, "")
			require.NoError(t, err)
			require.NoError(t, integrity.WriteManifest(dir, manifest))
		}
	}
	models, err := (&Connector{modelsDir: root}).ScanModels()
	require.NoError(t, err)
	require.Len(t, models, len(cases))
	byName := map[string]*metadata.ModelMetadata{}
	for _, m := range models {
		byName[m.Name] = m
	}
	for _, c := range cases {
		m := byName[c.repo]
		require.NotNil(t, m, c.repo)
		assert.Equal(t, c.repo != "org/complete", m.Incomplete, c.repo)
		assert.Equal(t, m.Incomplete, m.ToOllamaFormat("node")["incomplete"] == true)
	}
	assert.Equal(t, metadata.FormatMLX, byName[cases[1].repo].Format)
}

func TestScanModels_ManifestOnlyRepositoryIsIncomplete(t *testing.T) {
	for _, kind := range []string{"pending", "metadata", "malformed", "feature-only"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "org", "model")
			require.NoError(t, integrity.RecordPending(dir, "org/model", "", integrity.FileChecksum{RelativePath: "model.safetensors", Size: 8}))
			if kind == "metadata" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "generation_config.json"), []byte("{}"), 0600))
			}
			if kind == "feature-only" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "projector.gguf"), []byte("GGUF"), 0600))
				require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: "projector.gguf", Size: 4, Feature: "vision"}))
			}
			if kind == "malformed" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, integrity.ManifestFileName), []byte("{"), 0600))
			}
			unrelated := filepath.Join(root, "org", "unrelated")
			require.NoError(t, os.MkdirAll(unrelated, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(unrelated, "generation_config.json"), []byte("{}"), 0600))
			models, err := (&Connector{modelsDir: root}).ScanModels()
			require.NoError(t, err)
			require.Len(t, models, 1)
			require.Equal(t, "org/model", models[0].Name)
			require.Equal(t, dir, models[0].FullPath)
			require.True(t, models[0].Incomplete)
		})
	}
}
