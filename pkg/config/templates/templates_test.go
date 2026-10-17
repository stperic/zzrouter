package templates_test

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
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

// Tiers merge by flag name, so a key declared in another spelling could
// never be set: the Go spine and every shipped schema.yaml, which LoadFromFS
// merges per provider, declare flag names, overlays included.
func TestDeclaredParametersAreFlagNames(t *testing.T) {
	schemas, errs := pschema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	require.Subset(t, slices.Collect(maps.Keys(schemas)), slices.Collect(maps.Keys(pschema.Registry)))
	for provider, declared := range schemas {
		sites := map[string]map[string]pschema.ParamShape{"parameters": declared.Parameters}
		for endpoint, overlay := range declared.Endpoints {
			sites["endpoints."+endpoint+".parameters"] = overlay.Parameters
		}
		for site, shapes := range sites {
			for key := range shapes {
				assert.Equal(t, config.FlagName(key), key, "%s %s declares %q", provider, site, key)
			}
		}
	}
}

// launchedProviders loads the shipped tree as a node does and keeps the
// providers zzRouter starts as a process; a served daemon takes no flags.
func launchedProviders(t *testing.T) map[string]config.ServiceConfig {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	launched := map[string]config.ServiceConfig{}
	apps.RangeApps(func(name string, svc config.ServiceConfig) bool {
		if svc.Runtime != nil && len(svc.Runtime.Execution.Args) > 0 {
			launched[name] = svc
		}
		return true
	})
	require.GreaterOrEqual(t, len(launched), 4, "expected the on-demand providers")
	return launched
}

// A parameter is appended after the runtime's own argv; one that renders a
// flag the runtime already passes would fight the value zzRouter owns.
func TestShippedParametersNeverRenderARuntimeFlag(t *testing.T) {
	schemas, errs := pschema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	weights := filepath.Join(t.TempDir(), "weights")
	require.NoError(t, os.WriteFile(weights, nil, 0o600))
	model := process.ResolvedModel{Name: "model", Path: weights}

	for name, svc := range launchedProviders(t) {
		declared := maps.Clone(schemas[name].Parameters)
		for _, endpoint := range schemas[name].Endpoints {
			maps.Copy(declared, endpoint.Parameters)
		}
		executions := map[string]config.ExecutionConfig{"runtime.execution": svc.Runtime.Execution}
		for feature, f := range svc.Features {
			if f.Execution != nil {
				executions["features."+feature+".execution"] = *f.Execution
			}
		}
		for where, execution := range executions {
			launch := svc
			runtime := *svc.Runtime
			runtime.Execution = execution
			launch.Runtime = &runtime
			builder := process.NewCommandBuilder(&launch)
			base, err := builder.Build(model, 1, nil, declared)
			require.NoError(t, err, "%s: %s", name, where)
			passed := map[string]bool{}
			for _, arg := range base.Args {
				if strings.HasPrefix(arg, "--") {
					passed[arg] = true
				}
			}
			for key, shape := range declared {
				if runsettings.IsParameter(key) {
					continue // run settings never reach argv
				}
				values := []string{"true"}
				if shape.Negatable {
					values = append(values, "false")
				}
				for _, value := range values {
					built, err := builder.Build(model, 1, map[string]string{key: value}, declared)
					require.NoError(t, err, "%s: %s", name, key)
					flag := built.Args[len(base.Args)]
					assert.False(t, passed[flag], "%s: parameter %q=%s renders %s, which %s already passes", name, key, value, flag, where)
				}
			}
		}
	}
}

// A fresh install must never refuse its own config: every parameter a
// launched provider's shipped config sets in a tier, or names in a feature
// or as its context parameter, is declared for that endpoint.
func TestShippedConfigNamesOnlyDeclaredParameters(t *testing.T) {
	schemas, errs := pschema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)

	for name, svc := range launchedProviders(t) {
		declared := schemas[name]
		requireDeclared := func(where, endpoint string, keys []string) {
			shapes := declared.ForEndpoint(endpoint)
			for _, key := range keys {
				_, ok := shapes[key]
				assert.True(t, ok, "%s: %s names undeclared parameter %q", name, where, key)
			}
		}
		type cell struct {
			parameters map[string]string
			endpoints  map[string]config.EndpointOverlay
		}
		cells := map[string]cell{}
		if d := svc.Defaults; d != nil {
			cells["defaults"] = cell{d.Parameters, d.Endpoints}
			if d.Metadata != nil && d.Metadata.ContextParam != "" {
				requireDeclared("defaults.metadata.context_param", "", []string{d.Metadata.ContextParam})
			}
		}
		for model, spec := range svc.Models {
			cells["models."+model] = cell{spec.Parameters, spec.Endpoints}
		}
		for node, spec := range svc.Nodes {
			cells["nodes."+node] = cell{spec.Parameters, spec.Endpoints}
			for model, nodeModel := range spec.Models {
				cells["nodes."+node+".models."+model] = cell{nodeModel.Parameters, nodeModel.Endpoints}
			}
		}
		for where, c := range cells {
			requireDeclared(where, "", slices.Collect(maps.Keys(c.parameters)))
			for endpoint, overlay := range c.endpoints {
				requireDeclared(where+".endpoints."+endpoint, endpoint, slices.Collect(maps.Keys(overlay.Parameters)))
			}
		}
		for feature, f := range svc.Features {
			endpoint := config.FeatureEndpoint(feature)
			requireDeclared("features."+feature+".requires_parameters", endpoint, f.RequiresParameters)
			requireDeclared("features."+feature+".exclude_parameters", endpoint, f.ExcludeParameters)
		}
	}
}

func TestAppsFS_DeclaresRecurrentAllocationRows(t *testing.T) {
	providers, errs := pschema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	// vLLM 0.29 sizes its hybrid state pool by max-num-seqs through a version-specific layer grouping; it declares no rows.
	assert.Nil(t, providers["vllm"].RecurrentStateRows)
	assert.Equal(t, int64(1), *providers["mlx"].RecurrentStateRows)
	for _, endpoint := range []string{"chat", "embeddings", "reranking"} {
		slots := providers["llamacpp"].ForEndpoint(endpoint)["parallel"]
		require.True(t, slots.SequenceSlots)
		require.NotNil(t, slots.RecurrentStateRows)
		assert.Equal(t, int64(4), *slots.RecurrentStateRows)
	}
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
