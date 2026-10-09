package metadata

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrentCatalogConversionPreservesSourceEvidence(t *testing.T) {
	details := map[string]any{"format": "source", "vision": true}
	model := &ModelMetadata{Format: "gguf", Quantization: "Q4_K_M", Extra: map[string]any{"details": details}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				out := model.ToOllamaFormat("worker")["details"].(map[string]any)
				out["vision"] = false
			}
		})
	}
	wg.Wait()
	require.Equal(t, map[string]any{"format": "source", "vision": true}, details)
}
