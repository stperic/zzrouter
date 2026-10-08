package templates_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
)

// The JSON schema gates shape, but config.VersionSource.Validate is strictly
// stronger: it rejects pypi+semver, charset-checks repo, bounds identifier
// length, and rejects package on a github source. Without this walk a
// template carrying `type: pypi, compare: semver` ships green and only fails
// at runtime, on the node, in the field.
func TestShippedTemplatesPassVersionSourceValidate(t *testing.T) {
	const root = "files/providers"

	var checked int
	err := fs.WalkDir(templates.AppsFS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(p) != "config.yaml" {
			return walkErr
		}

		data, readErr := fs.ReadFile(templates.AppsFS, p)
		require.NoError(t, readErr, p)

		var doc struct {
			VersionSource *config.VersionSource `yaml:"version_source"`
		}
		require.NoError(t, yaml.Unmarshal(data, &doc), p)

		if doc.VersionSource == nil {
			return nil // not every provider is installable
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		assert.NoError(t, doc.VersionSource.Validate(), "template %s declares an invalid version_source", rel)
		checked++
		return nil
	})
	require.NoError(t, err)

	// Guard the guard: if the walk silently stops matching, this test would
	// pass while checking nothing.
	assert.GreaterOrEqual(t, checked, 4, "expected every installable provider to declare version_source")
}

// WalkDir visits lexically, so cloud/ is reached before external/ and
// on-demand/. Before the fold, one malformed cloud config aborted the whole
// walk, which silently withheld version_source from exactly the two
// providers whose installers now require it.
func TestReconcileContinuesPastAnUnreconcilableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	// Corrupt a provider that sorts before external/ and on-demand/.
	bad := filepath.Join(dir, "cloud", "anthropic", "config.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("this: [is not: valid yaml\n"), 0o600))

	// Strip version_source from the two installable providers so reconcile
	// has something to restore.
	for _, rel := range []string{
		filepath.Join("on-demand", "llamacpp", "config.yaml"),
		filepath.Join("external", "ollama", "config.yaml"),
	} {
		p := filepath.Join(dir, rel)
		data, err := os.ReadFile(p) //nolint:gosec // test-owned temp path
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(data, &doc))
		delete(doc, "version_source")
		out, err := yaml.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, out, 0o600))
	}

	_, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err, "one bad file must not fail the whole reconcile")

	for _, rel := range []string{
		filepath.Join("on-demand", "llamacpp", "config.yaml"),
		filepath.Join("external", "ollama", "config.yaml"),
	} {
		data, readErr := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // test-owned temp path
		require.NoError(t, readErr)
		var doc struct {
			VersionSource *config.VersionSource `yaml:"version_source"`
		}
		require.NoError(t, yaml.Unmarshal(data, &doc))
		require.NotNil(t, doc.VersionSource, "%s never received version_source", rel)
		assert.NoError(t, doc.VersionSource.Validate())
	}
}
