package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsOllamaCloudVariant(t *testing.T) {
	// Cloud variants
	assert.True(t, isOllamaCloudVariant("minimax-m2.7", "cloud"))
	assert.True(t, isOllamaCloudVariant("minimax-m2.7", "Cloud"))
	assert.True(t, isOllamaCloudVariant("minimax-m2.7:cloud", ""))
	assert.True(t, isOllamaCloudVariant("model:something-cloud", ""))

	// Local variants
	assert.False(t, isOllamaCloudVariant("llama3", ""))
	assert.False(t, isOllamaCloudVariant("llama3", "latest"))
	assert.False(t, isOllamaCloudVariant("llama3:8b", ""))
	assert.False(t, isOllamaCloudVariant("llama3", "q4_0"))
	assert.False(t, isOllamaCloudVariant("cloudflare-model", ""))
}
