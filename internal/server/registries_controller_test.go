package server

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
)

func TestParseQuant(t *testing.T) {
	cases := []struct {
		filename string
		wantTag  string
		wantBits int
	}{
		{"SmolLM2-135M-Instruct-Q4_K_M.gguf", "Q4_K_M", 4},
		{"Llama-3.2-3B-Instruct.Q5_K_M.gguf", "Q5_K_M", 5},
		{"model.Q8_0.gguf", "Q8_0", 8},
		{"model.F16.gguf", "F16", 16},
		{"model.BF16.gguf", "BF16", 16},
		{"model.IQ3_XS.gguf", "IQ3_XS", 3},
		{"model-q4_k_m.gguf", "Q4_K_M", 4}, // case-insensitive
		{"model.gguf", "", 0},              // no quant tag
		{"model.safetensors", "", 0},       // not gguf (caller filters; parseQuant is still safe)
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			tag, bits := parseQuant(tc.filename)
			assert.Equal(t, tc.wantTag, tag)
			assert.Equal(t, tc.wantBits, bits)
		})
	}
}

func TestMarkRecommended_PrefersQ4_K_M(t *testing.T) {
	vs := []GGUFVariant{
		{File: "a.Q8_0.gguf", Quantization: "Q8_0", Bits: 8, SizeBytes: 200},
		{File: "b.Q4_K_M.gguf", Quantization: "Q4_K_M", Bits: 4, SizeBytes: 100},
		{File: "c.F16.gguf", Quantization: "F16", Bits: 16, SizeBytes: 400},
	}
	markRecommended(vs)
	assert.False(t, vs[0].Recommended)
	assert.True(t, vs[1].Recommended)
	assert.Contains(t, vs[1].RecommendationReason, "balanced")
	assert.False(t, vs[2].Recommended)
}

func TestMarkRecommended_FallsBackToSmallestGe4Bits(t *testing.T) {
	vs := []GGUFVariant{
		{File: "a.Q2_K.gguf", Quantization: "Q2_K", Bits: 2, SizeBytes: 50},
		{File: "b.Q5_K_M.gguf", Quantization: "Q5_K_M", Bits: 5, SizeBytes: 150},
		{File: "c.Q8_0.gguf", Quantization: "Q8_0", Bits: 8, SizeBytes: 300},
	}
	markRecommended(vs)
	assert.False(t, vs[0].Recommended)
	assert.True(t, vs[1].Recommended, "Q5_K_M is the smallest ≥4-bit variant")
	assert.False(t, vs[2].Recommended)
}

func TestMarkRecommended_AllSub4Bits(t *testing.T) {
	vs := []GGUFVariant{
		{File: "a.Q2_K.gguf", Quantization: "Q2_K", Bits: 2, SizeBytes: 50},
		{File: "b.Q3_K_M.gguf", Quantization: "Q3_K_M", Bits: 3, SizeBytes: 75},
	}
	markRecommended(vs)
	assert.False(t, vs[0].Recommended)
	assert.True(t, vs[1].Recommended, "largest is picked when no ≥4-bit variant exists")
}

func TestBuildVariantsResponse_FiltersNonGGUF(t *testing.T) {
	files := []metadata.TreeFileEntry{
		{Name: "README.md", Size: 1024},
		{Name: "config.json", Size: 512},
		{Name: "model.Q4_K_M.gguf", Size: 100_000_000},
		{Name: "model.Q8_0.gguf", Size: 200_000_000},
	}
	resp := buildVariantsResponse("org/repo", files)
	assert.Equal(t, "org/repo", resp.ModelID)
	assert.Len(t, resp.Variants, 2)
	assert.Equal(t, 2, resp.NonGGUFFiles)
	assert.Equal(t, int64(300_001_536), resp.TotalSizeBytes)
	// Q4_K_M should be recommended
	for _, v := range resp.Variants {
		if v.Quantization == "Q4_K_M" {
			assert.True(t, v.Recommended)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := map[int64]string{
		512:                    "512 B",
		2048:                   "2.0 KB",
		5 * 1024 * 1024:        "5.0 MB",
		3 * 1024 * 1024 * 1024: "3.0 GB",
	}
	for n, want := range cases {
		assert.Equal(t, want, humanizeBytes(n))
	}
}

func TestListHuggingFaceVariants_BadRequestBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctrl := NewRegistriesController(func() *modelregistry.Registry { return nil })

	t.Run("missing id", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/registries/huggingface/variants", nil)
		ctrl.ListHuggingFaceVariants(c)
		assert.Equal(t, 400, w.Code)
		assert.Contains(t, w.Body.String(), "'id' is required")
	})

	t.Run("malformed id (no slash)", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/registries/huggingface/variants?id=bogus", nil)
		ctrl.ListHuggingFaceVariants(c)
		assert.Equal(t, 400, w.Code)
		assert.Contains(t, w.Body.String(), "org/repo")
	})
}
