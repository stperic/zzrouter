package install

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func releasedSource(t *testing.T) (config.ServiceConfig, RecipeSnapshot) {
	t.Helper()
	sc := shippedRecipeFixture(t, "comfyui")
	sc.Runtime = &config.AppRuntimeConfig{DataDir: "data", Execution: config.ExecutionConfig{Type: "python", WorkingDir: "source", Args: []string{"main.py"}}}
	snapshot, err := ResolveRecipe(sc, "worker", "comfyui", "", nil)
	require.NoError(t, err)
	return sc, snapshot
}

func parsedAdditions(t *testing.T, constraint string) (InstallPolicy, string) {
	t.Helper()
	policy, fp, err := parseInstallPolicy([]byte("runtimes:\n  comfyui/comfyui:\n    additional_packages:\n      torchaudio: '" + constraint + "'\n"))
	require.NoError(t, err)
	return policy, fp
}

func TestAdditionalPackagesPolicySyntaxAndBounds(t *testing.T) {
	for _, size := range []int{60, 128, 129} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			packages := map[string]string{}
			for n := range size {
				packages[fmt.Sprintf("extension-%d", n)] = ">=1,<2"
			}
			body, err := yaml.Marshal(map[string]any{"runtimes": map[string]any{"engine/engine": map[string]any{"additional_packages": packages}}})
			require.NoError(t, err)
			_, _, err = parseInstallPolicy(body)
			if size > AdditionalPackagesLimit {
				require.ErrorContains(t, err, "128")
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, fields := range []string{
		"additional_packages: {Torchaudio: ''}",
		"additional_packages: {torchaudio: 'https://example.org/package.whl'}",
		"additional_packages: {'package[extra]': ''}",
		"additional_packages: {package: '>=1; python_version < 3'}",
		"additional_packages: {torchaudio: ''}\n    packages: {torch: ''}",
		"additional_packages: {torchaudio: ''}\n    allow_source_builds: true",
		"additional_packages: {torchaudio: ''}\n    indexes: [https://example.org/simple]",
		"additional_packages: {torchaudio: ''}\n    imports: [torchaudio]",
		"additional_packages: null",
		"unrecognized: {}",
	} {
		_, _, err := parseInstallPolicy([]byte("runtimes:\n  engine/engine:\n    " + fields + "\n"))
		require.Error(t, err, fields)
	}
	_, _, err := parseInstallPolicy([]byte("runtimes:\n  engine/engine:\n    additional_packages: {}\n"))
	require.NoError(t, err, "explicit empty selection must revoke additions")
}

func TestAdditionalPackagesAuthorizeOnlyReleasedSourceRoots(t *testing.T) {
	_, snapshot := releasedSource(t)
	policy, _ := parsedAdditions(t, ">=2,<3")
	grant := policy.Runtimes["comfyui/comfyui"]
	snapshot.AdditionalPackages = map[string]string{"torchaudio": ">=2,<3"}
	require.NoError(t, grant.Authorize(snapshot, nil))
	snapshot.AdditionalPackages["unapproved"] = ""
	require.ErrorContains(t, grant.Authorize(snapshot, nil), "unapproved")
	delete(snapshot.AdditionalPackages, "unapproved")
	snapshot.Authority = ""
	require.ErrorContains(t, grant.Authorize(snapshot, nil), "release-owned")
	pip := RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: shippedRecipe(t, "vllm", "vllm")}}
	require.ErrorContains(t, grant.Authorize(pip, nil), "source runtime")
}

func TestSourceAdditionsCannotAuthorizeSourceOverrides(t *testing.T) {
	sc, _ := releasedSource(t)
	for _, field := range []string{"url", "sha256", "version"} {
		t.Run(field, func(t *testing.T) {
			changed := sc
			r := sc.Install.Runtimes["comfyui"]
			source := *r.Source
			switch field {
			case "url":
				source.URL = "https://github.com/example/engine/archive/v0.40.0.tar.gz"
			case "sha256":
				source.SHA256 = strings.Repeat("a", 64)
			case "version":
				source.Version = "99.0.0"
			}
			r.Source = &source
			changed.Install = &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{"comfyui": r}}
			_, err := ResolveRecipe(changed, "worker", "comfyui", "/nonexistent/policy", nil)
			require.ErrorContains(t, err, "unchanged shipped declaration", "refuse before any policy can admit a source")
			changed.Install = sc.Install
			changed.Defaults = &config.AppDefaultsConfig{Install: &config.InstallConfig{Runtimes: map[string]config.InstallRecipe{"comfyui": {}}}}
			_, err = ResolveRecipe(changed, "worker", "comfyui", "/nonexistent/policy", nil)
			require.ErrorContains(t, err, "overrides are forbidden")
		})
	}
	_, err := ResolveRecipe(sc, "worker", "comfyui", t.TempDir()+"/policy", nil)
	require.ErrorIs(t, err, ErrInstallPolicy, "configured unsafe/missing policy must fail closed")
	manifest := &fsroot.InstallManifest{AdditionalPackages: map[string]string{"torchaudio": ""}}
	_, err = ResolveRecipe(sc, "worker", "comfyui", "", manifest)
	require.ErrorIs(t, err, ErrInstallPolicy, "installed additions never acquire release-only authority")
}

func TestAdditionalPackagesInstalledVersionsAndPolicyRecheck(t *testing.T) {
	_, snapshot := releasedSource(t)
	policy, fp := parsedAdditions(t, ">=2,<3")
	snapshot.PolicyFingerprint = fp
	snapshot.AdditionalPackages = map[string]string{"torchaudio": "==2.5"}
	inventory := []ResolvedPackage{{Name: "torch", Version: "2.5", Requested: true}, {Name: "torchaudio", Version: "2.5", Requested: true}}
	require.NoError(t, snapshot.recheckPolicy(policy, fp, inventory), "release roots retain release authority")
	for _, failure := range []string{"missing", "not requested", "recorded constraint", "current constraint"} {
		t.Run(failure, func(t *testing.T) {
			entries := append([]ResolvedPackage{}, inventory...)
			grant := policy.Runtimes["comfyui/comfyui"]
			switch failure {
			case "missing":
				entries = entries[:1]
			case "not requested":
				entries[1].Requested = false
			case "recorded constraint":
				entries[1].Version = "2.6"
			case "current constraint":
				fresh, _ := parsedAdditions(t, ">=2.6,<3")
				grant = fresh.Runtimes["comfyui/comfyui"]
			}
			require.ErrorIs(t, grant.Authorize(snapshot, entries), ErrInstallPolicy)
		})
	}
	for _, change := range []string{"add", "remove", "constraint", "unrelated"} {
		t.Run(change, func(t *testing.T) {
			fresh, _ := parsedAdditions(t, ">=2,<3")
			grant := fresh.Runtimes["comfyui/comfyui"]
			switch change {
			case "add":
				grant.AdditionalPackages["other"] = ""
			case "remove":
				delete(grant.AdditionalPackages, "torchaudio")
			case "constraint":
				grant.AdditionalPackages["torchaudio"] = ">=2.6"
			case "unrelated":
				fresh.Runtimes["other/other"] = RuntimePolicy{Packages: map[string]string{"other": ""}}
			}
			fresh.Runtimes["comfyui/comfyui"] = grant
			require.ErrorContains(t, snapshot.recheckPolicy(fresh, Fingerprint(fresh), inventory), "policy changed; preview again")
		})
	}
	snapshot.AdditionalPackages = nil
	require.NoError(t, snapshot.recheckPolicy(policy, fp, []ResolvedPackage{}), "new desired additions must not become installed probes")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, CheckRecipeAuthority(ctx, snapshot, nil), context.Canceled)
}
