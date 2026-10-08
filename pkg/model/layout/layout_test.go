package layout

import (
	"testing"

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
