package views

import (
	"testing"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/stretchr/testify/assert"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

func TestFilterInstancesByModel(t *testing.T) {
	instances := []pkgClient.Instance{
		{App: "ollama", Model: "llama3", Node: "node1"},
		{App: "vllm", Model: "qwen2", Node: "node1"},
		{App: "ollama", Model: "phi3", Node: "node2"},
	}

	filtered := shared.FilterInstances(instances, "", "qwen2")
	assert.Len(t, filtered, 1)
	assert.Equal(t, "qwen2", filtered[0].Model)

	filtered = shared.FilterInstances(instances, "", "")
	assert.Len(t, filtered, 3)
}

func TestSortModelRowsByNameAndSize(t *testing.T) {
	models := []pkgClient.ModelMetadata{
		{Name: "beta", Size: 100},
		{Name: "alpha", Size: 200},
	}

	shared.SortModelRows(models, "model")
	assert.Equal(t, "alpha", models[0].Name)

	shared.SortModelRows(models, "size")
	assert.Equal(t, int64(200), models[0].Size)
}
