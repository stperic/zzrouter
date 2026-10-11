package layout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func names(vs []Variant) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Name
	}
	return out
}

func TestComplete_IndexedWeightsAndFeatures(t *testing.T) {
	for _, ext := range []string{"safetensors", "bin"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			first, second := "model-00001."+ext, "model-00002."+ext
			index := "model." + ext + ".index.json"
			require.NoError(t, os.WriteFile(filepath.Join(dir, first), []byte("weights"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, index), []byte(`{"weight_map":{"a":"`+first+`","b":"`+second+`"}}`), 0o600))
			complete, err := Complete(dir)
			require.NoError(t, err)
			assert.False(t, complete)
			complete, err = Complete(filepath.Join(dir, first))
			require.NoError(t, err)
			assert.False(t, complete)
			require.NoError(t, os.WriteFile(filepath.Join(dir, second), []byte("weights"), 0o600))
			complete, err = Complete(dir)
			require.NoError(t, err)
			assert.True(t, complete)
			require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: second, Feature: "vision", Size: 7}))
			complete, err = Complete(dir)
			require.NoError(t, err)
			assert.False(t, complete, "feature files cannot supply a required weights shard")
		})
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.npz"), []byte("weights"), 0o600))
	complete, err := Complete(dir)
	require.NoError(t, err)
	assert.True(t, complete)
}

// A split GGUF is one variant, in shard order, whether it sits at the top
// or in a quant subdirectory, and a set missing a shard is not offered.
func TestGGUFVariants_GroupsShards(t *testing.T) {
	files := []File{
		{Path: "README.md", Size: 1},
		{Path: "Qwen-Q4_K_M.gguf", Size: 10},
		{Path: "Q8_0/Qwen-Q8_0-00002-of-00002.gguf", Size: 20},
		{Path: "Q8_0/Qwen-Q8_0-00001-of-00002.gguf", Size: 30},
		{Path: "BF16/Qwen-BF16-00001-of-00003.gguf", Size: 1},
		{Path: "BF16/Qwen-BF16-00003-of-00003.gguf", Size: 1},
	}
	vs := GGUFVariants(files)
	require.Equal(t, []string{"Qwen-Q8_0", "Qwen-Q4_K_M"}, names(vs))
	assert.Equal(t, "Q8_0/Qwen-Q8_0-00001-of-00002.gguf", vs[0].Files[0].Path, "the engine is pointed at the first shard")
	assert.Len(t, vs[0].Files, 2)
	assert.Equal(t, int64(50), vs[0].Size())
}

func TestPick(t *testing.T) {
	vs := GGUFVariants([]File{
		{Path: "Qwen-Q4_K_M.gguf"}, {Path: "Qwen-UD-Q4_K_M.gguf"},
		{Path: "Q8_0/Qwen-Q8_0-00001-of-00002.gguf"}, {Path: "Q8_0/Qwen-Q8_0-00002-of-00002.gguf"},
	})
	cases := []struct{ hint, want string }{
		{"Qwen-UD-Q4_K_M.gguf", "Qwen-UD-Q4_K_M"},
		{"q8_0/qwen-q8_0-00002-of-00002.gguf", "Qwen-Q8_0"},
		{"Q4_K_M", "Qwen-Q4_K_M"},
		{"q8_0", "Qwen-Q8_0"},
		{"Q4_K", "Qwen-Q4_K_M"}, // a fragment takes the first match by path
	}
	for _, c := range cases {
		v, ok := Pick(vs, c.hint)
		require.True(t, ok, c.hint)
		assert.Equal(t, c.want, v.Name, c.hint)
	}
	_, ok := Pick(vs, "Q2_K")
	assert.False(t, ok)
	_, ok = Pick(vs, "")
	assert.False(t, ok, "no hint among several variants picks nothing")
	one, ok := Pick(vs[:1], "")
	assert.True(t, ok)
	assert.Equal(t, vs[0].Name, one.Name)
}

// The globs state a preference: the first that matches anything wins,
// whatever order the files are listed in.
func TestFirstMatch_FollowsGlobOrder(t *testing.T) {
	files := []File{{Path: "mmproj-BF16.gguf"}, {Path: "mmproj-F32.gguf"}, {Path: "sub/mmproj-F16.gguf"}}
	f, ok := FirstMatch(files, []string{"mmproj-F16.gguf", "mmproj-*.gguf"})
	require.True(t, ok)
	assert.Equal(t, "sub/mmproj-F16.gguf", f.Path)

	f, ok = FirstMatch(files, []string{"mmproj-Q8.gguf", "mmproj-*.gguf"})
	require.True(t, ok)
	assert.Equal(t, "mmproj-BF16.gguf", f.Path)

	_, ok = FirstMatch(files, []string{"*.safetensors"})
	assert.False(t, ok)
}

// VariantOf finds the whole split set from any one shard, and refuses a
// shard whose set is incomplete.
func TestVariantOf(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"m-q4-00001-of-00002.gguf", "m-q4-00002-of-00002.gguf", "m-q8-00001-of-00002.gguf", "m-q2.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}

	v, ok := VariantOf(filepath.Join(dir, "m-q4-00002-of-00002.gguf"))
	require.True(t, ok)
	assert.Equal(t, "m-q4", v.Name)
	assert.Len(t, v.Files, 2)

	v, ok = VariantOf(filepath.Join(dir, "m-q2.gguf"))
	require.True(t, ok)
	assert.Equal(t, "m-q2", v.Name)

	_, ok = VariantOf(filepath.Join(dir, "m-q8-00001-of-00002.gguf"))
	assert.False(t, ok)
}

func TestWeightsExcludesFeaturesAndMetadata(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.safetensors", "b.safetensors", "projector.safetensors", "config.json", "tokenizer.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("contents"), 0600))
	}
	files, err := Weights(dir, []string{"projector.safetensors"})
	require.NoError(t, err)
	require.Equal(t, []File{{Path: "a.safetensors", Size: 8}, {Path: "b.safetensors", Size: 8}}, files)
}
