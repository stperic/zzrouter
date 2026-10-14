package install

import (
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnshippedSourceCannotAcquireInstallAuthority(t *testing.T) {
	// Keep all vLLM checks while replacing its pip mode, so the authority
	// refusal discriminates source ownership rather than a missing import.
	sc := shippedRecipeFixture(t, "vllm")
	r := sc.Install.Runtimes["vllm"]
	r.Package, r.StartupImport, r.VersionConstraint, r.Toolkit = nil, nil, nil, nil
	r.Source = &config.InstallSource{Version: "1.0.0", URL: "https://github.com/example/engine/archive/refs/tags/v1.0.0.tar.gz", SHA256: strings.Repeat("a", 64), StripComponents: 1, Requirements: "requirements.txt", Entrypoint: "main.py"}
	sc.VersionSource = nil
	sc.Runtime = &config.AppRuntimeConfig{DataDir: "data", Execution: config.ExecutionConfig{Type: "python", WorkingDir: "source", Args: []string{"main.py"}}}
	for _, name := range []string{"vllm", "unshipped-engine"} {
		t.Run(name, func(t *testing.T) {
			sc.Name = name
			sc.Install = &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{name: r}}
			snapshot, err := ResolveRecipe(sc, "worker", name, "")
			require.ErrorIs(t, err, ErrInstallPolicy)
			assert.Contains(t, err.Error(), "source recipes require an unchanged shipped declaration")
			assert.Empty(t, snapshot.Authority)
			assert.Equal(t, r.Source.Version, snapshot.PinnedVersion)
		})
	}
	grant := RuntimePolicy{Packages: map[string]string{"torch": "", "torchvision": ""}, Entrypoints: []string{"main.py"}, Indexes: []string{"https://pypi.org/simple"}}
	require.ErrorContains(t, grant.Authorize(r), "release-owned", "operator grants cannot admit source recipes")
}

func TestSourceRuntimeChecksHaveNoPipEntrypointPackage(t *testing.T) {
	imports, checks := []string{"torch", "torchvision"}, []string{"pip_check", "imports", "cuda_companions"}
	r := config.InstallRecipe{Source: &config.InstallSource{}, Verify: &config.InstallVerify{Imports: &imports, Checks: &checks}, Companions: map[string]config.InstallCompanion{"torchvision": {}, "torch": {}}}
	snapshot := RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: r}}
	out := snapshot.RuntimeChecks(schema.RuntimeChecks{Packages: []string{"stale-package"}})
	assert.Equal(t, []string{"torch", "torchvision"}, out.Packages)
	assert.Equal(t, imports, out.Imports)
	assert.Equal(t, checks, out.Checks)
	out.Imports[0] = "changed"
	assert.Equal(t, "torch", imports[0])
}
