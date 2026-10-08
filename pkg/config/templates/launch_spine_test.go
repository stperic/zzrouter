package templates_test

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
)

// TestShippedProvidersDeclareBothModelFacets is the guard that would have
// caught MLX being pointed at a bare repo id: every engine zzRouter
// launches must be told where the weights are (${MODEL_PATH}) and what it
// answers to on the wire (wire_model). One facet declared without the
// other is how an engine ends up quietly downloading its own copy.
func TestShippedProvidersDeclareBothModelFacets(t *testing.T) {
	launching := 0

	err := fs.WalkDir(templates.AppsFS, "files/providers", func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() || filepath.Base(p) != "config.yaml" {
			return nil
		}

		raw, readErr := fs.ReadFile(templates.AppsFS, p)
		require.NoError(t, readErr)

		var doc struct {
			Runtime *config.AppRuntimeConfig `yaml:"runtime"`
		}
		require.NoError(t, yaml.Unmarshal(raw, &doc), p)
		if doc.Runtime == nil || len(doc.Runtime.Execution.Args) == 0 {
			return nil // nothing is launched (cloud, external, registry)
		}

		exec := doc.Runtime.Execution
		if !strings.Contains(strings.Join(exec.Args, " "), config.PlaceholderModel) &&
			!strings.Contains(strings.Join(exec.Args, " "), config.PlaceholderModelPath) {
			return nil // launches a server that isn't per-model
		}
		launching++

		name := filepath.Base(filepath.Dir(p))
		assert.Contains(t, strings.Join(exec.Args, " "), config.PlaceholderModelPath,
			"%s must point the engine at local weights", name)
		assert.NotEmpty(t, exec.WireModel,
			"%s must declare wire_model — leaving it implicit is what hid the MLX bug", name)
		assert.NoError(t, config.ValidateExecutionCommand(name, exec), name)
		return nil
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, launching, 3, "expected the on-demand providers to be covered")
}
