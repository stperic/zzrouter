package templates_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/config/templates"
	pschema "github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// TestAppsFS_SchemaYAMLMatchesGoSpine walks the embedded templates tree
// and merges every schema.yaml onto the Go spine. A conflict between a
// YAML-declared type and the Go-declared kind is a structural bug
// (plan §4.4) and must surface as a hard error before any binary ships.
func TestAppsFS_SchemaYAMLMatchesGoSpine(t *testing.T) {
	_, errs := pschema.LoadFromFS(templates.AppsFS, "files/providers")
	assert.Empty(t, errs, "schema.yaml ↔ Go spine drift: %v", errs)
}

// Drift-guard: every per-provider YAML under AppsFS must validate against
// its kind's schema. If a provider file adds a field the schema forbids,
// or vice versa, this test catches it on every CI run.
func TestAppsFS_AllFilesValidatePerKindSchema(t *testing.T) {
	validators := map[string]func([]byte) error{
		"on-demand":  schema.ValidateOnDemand,
		"external":   schema.ValidateExternal,
		"cloud":      schema.ValidateCloud,
		"registries": schema.ValidateRegistry,
	}

	err := fs.WalkDir(templates.AppsFS, "files/providers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		// settings.yaml is not a provider.
		if filepath.Base(path) == "settings.yaml" {
			return nil
		}
		// Walk path layout: files/providers/<kind>/<name>/config.yaml.
		// schema.yaml siblings are skipped — they are consumed by
		// pkg/prov_apps/schema, not the per-kind JSON schema validator.
		rel := strings.TrimPrefix(path, "files/providers/")
		parts := strings.Split(rel, "/")
		base := parts[len(parts)-1]
		if base == "schema.yaml" {
			return nil
		}
		require.Len(t, parts, 3, "unexpected layout: %s", path)
		require.Equal(t, "config.yaml", base, "per-provider file must be config.yaml: %s", path)
		kind := parts[0]
		validator, ok := validators[kind]
		require.Truef(t, ok, "no validator for kind %q in %s", kind, path)

		content, err := fs.ReadFile(templates.AppsFS, path)
		require.NoError(t, err)
		assert.NoErrorf(t, validator(content), "validation failed for %s", path)
		return nil
	})
	require.NoError(t, err)
}

// TestInstallDefaults_CopiesAllFilesAndPreservesEdits verifies the
// install path on a clean directory copies every embedded file, and a
// second call leaves any user-edited files untouched (copy-if-missing).
func TestInstallDefaults_CopiesAllFilesAndPreservesEdits(t *testing.T) {
	dst := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dst))

	// Every file in the embedded tree must exist on disk under its
	// corresponding relative path.
	want := []string{}
	require.NoError(t, fs.WalkDir(templates.AppsFS, "files/providers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, "files/providers/")
		want = append(want, rel)
		return nil
	}))
	require.NotEmpty(t, want, "expected embedded provider files to enumerate")

	for _, rel := range want {
		fp := filepath.Join(dst, filepath.FromSlash(rel))
		_, err := os.Stat(fp)
		assert.NoErrorf(t, err, "missing on disk after InstallDefaults: %s", rel)
	}

	// Pick one file, mutate it, re-run InstallDefaults, confirm the
	// edit survives.
	target := filepath.Join(dst, filepath.FromSlash(want[0]))
	require.NoError(t, os.WriteFile(target, []byte("# user edit\n"), 0644))
	require.NoError(t, templates.InstallDefaults(dst))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "# user edit\n", string(got), "InstallDefaults must not overwrite existing files")
}
