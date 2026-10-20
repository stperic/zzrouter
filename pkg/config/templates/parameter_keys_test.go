package templates_test

import (
	"io/fs"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func TestShippedAndSpineParameterKeys(t *testing.T) {
	check := func(t *testing.T, key string) {
		t.Helper()
		assert.True(t, runsettings.ValidKey(key), "invalid declared key %q", key)
		assert.NoError(t, process.ValidateParameters(map[string]string{key: ""}), "declared key %q must launch", key)
	}
	for provider, shapes := range schema.Registry {
		t.Run("spine/"+provider, func(t *testing.T) {
			for key := range shapes {
				check(t, key)
			}
		})
	}
	files := 0
	err := fs.WalkDir(templates.AppsFS, "files/providers", func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() || path.Base(p) != "schema.yaml" {
			return nil
		}
		files++
		t.Run(p, func(t *testing.T) {
			data, err := templates.AppsFS.ReadFile(p)
			require.NoError(t, err)
			declared, err := schema.LoadYAMLSchema(data)
			require.NoError(t, err)
			for key := range declared.Parameters {
				check(t, key)
			}
			for endpoint, overlay := range declared.Endpoints {
				t.Run(endpoint, func(t *testing.T) {
					for key := range overlay.Parameters {
						check(t, key)
					}
				})
			}
		})
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, files, "must check shipped schemas")
}
