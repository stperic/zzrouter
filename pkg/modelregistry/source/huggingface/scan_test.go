package huggingface

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/integrity"
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
