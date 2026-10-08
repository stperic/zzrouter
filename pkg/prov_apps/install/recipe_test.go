package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func shippedRecipeFixture(t *testing.T, provider string) config.ServiceConfig {
	t.Helper()
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + provider + "/config.yaml")
	require.NoError(t, err)
	var p config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(data, &p))
	return config.ServiceConfig{Name: provider, Install: p.Install, Requirements: p.Requirements, VersionSource: p.VersionSource, PinnedVersion: p.PinnedVersion, Nodes: map[string]config.NodeSpec{}}
}

func TestReleaseAuthorityDoesNotNeedOperatorPolicy(t *testing.T) {
	for _, tc := range []struct{ provider, runtime string }{{"vllm", "vllm"}, {"mlx", "mlx"}, {"mlx", "mlx-vlm"}} {
		t.Run(tc.runtime, func(t *testing.T) {
			sc := shippedRecipeFixture(t, tc.provider)
			snapshot, err := ResolveRecipe(sc, "worker", tc.runtime, "")
			require.NoError(t, err)
			assert.Equal(t, "release", snapshot.Authority)
			assert.Nil(t, snapshot.Policy)
			// A remote override is storable without granting this node's installation authority.
			override, err := config.MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"`+tc.runtime+`":{"timeout":"40m"}}}`))
			require.NoError(t, err)
			sc.Nodes["remote"] = config.NodeSpec{Install: override}
			_, err = ResolveRecipe(sc, "worker", tc.runtime, "")
			require.NoError(t, err)
			_, err = ResolveRecipe(sc, "remote", tc.runtime, "")
			assert.ErrorIs(t, err, ErrInstallPolicy)
		})
	}
}

func TestRestatingShippedValueRequiresProtectedPolicy(t *testing.T) {
	sc := shippedRecipeFixture(t, "mlx")
	override, err := config.MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"mlx":{"timeout":"30m"}}}`))
	require.NoError(t, err)
	sc.Defaults = &config.AppDefaultsConfig{Install: override}
	_, err = ResolveRecipe(sc, "worker", "mlx", "")
	assert.ErrorIs(t, err, ErrInstallPolicy)
	sc.Defaults.Install = nil
	_, err = ResolveRecipe(sc, "worker", "mlx", "")
	require.NoError(t, err)
	base := sc.Install.Runtimes["mlx"]
	*base.Timeout = "40m"
	_, err = ResolveRecipe(sc, "worker", "mlx", "")
	assert.ErrorIs(t, err, ErrInstallPolicy, "writable base cannot acquire release authority")
}

func TestPolicyCannotApproveRemovingRequiredStartupImport(t *testing.T) {
	sc := shippedRecipeFixture(t, "vllm")
	override, err := config.MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"vllm":{"verify":{"imports":["vllm.entrypoints.openai.api_server"]}}}}`))
	require.NoError(t, err)
	sc.Defaults = &config.AppDefaultsConfig{Install: override}
	_, err = ResolveRecipe(sc, "worker", "vllm", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be removed")
}

func TestOverridePolicyRejectsServiceOwnedAndSymlinkPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("runtimes: {}\n"), 0600))
	_, _, err := ReadInstallPolicy(path)
	require.Error(t, err)
	link := path + ".link"
	require.NoError(t, os.Symlink(path, link))
	_, _, err = ReadInstallPolicy(link)
	require.Error(t, err)
}

func TestAcceptedAuthorityRechecksWithoutUsingPatchedDesiredRecipe(t *testing.T) {
	sc := shippedRecipeFixture(t, "mlx")
	snapshot, err := ResolveRecipe(sc, "worker", "mlx", "")
	require.NoError(t, err)
	require.NoError(t, CheckRecipeAuthority(t.Context(), snapshot, func(string, string) (RecipeSnapshot, error) {
		t.Fatal("accepted job must not re-read mutable recipe")
		return RecipeSnapshot{}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, CheckRecipeAuthority(ctx, snapshot, nil), context.Canceled)
}

func TestDependencyReportProvenance(t *testing.T) {
	fixture := `{"version":"1","install":[{"metadata":{"name":"engine","version":"1.0"},"requested":true,"is_direct":false,"download_info":{"url":"https://files.pythonhosted.org/packages/engine.whl","archive_info":{"hashes":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}]}`
	roots := map[string]string{"engine": "==1.0"}
	inventory, err := ParsePipReport([]byte(fixture), roots)
	require.NoError(t, err)
	require.Len(t, inventory, 1)
	assert.Contains(t, InventoryRequirements(inventory), "engine==1.0 --hash=sha256:")
	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(fixture), &data))
	entry := data["install"].([]any)[0].(map[string]any)
	entry["is_direct"] = true
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	_, err = ParsePipReport(raw, roots)
	assert.Error(t, err)
	entry["is_direct"] = false
	entry["download_info"].(map[string]any)["url"] = "http://untrusted.test/engine.whl"
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	_, err = ParsePipReport(raw, roots)
	assert.Error(t, err)
	url := "https://download.pytorch.org/whl/cu130/engine-1.0%2Bcu130-cp311-cp311-linux_x86_64.whl"
	entry["download_info"].(map[string]any)["url"] = url
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	inventory, err = ParsePipReport(raw, roots)
	require.NoError(t, err)
	assert.Equal(t, url, inventory[0].ArtifactURL)
	assert.NotContains(t, InventoryRequirements(inventory), url, "artifact URLs are provenance, never command input")
}

func TestRecipeRequiresActualPythonServingModule(t *testing.T) {
	for _, runtime := range []string{"mlx", "mlx-vlm"} {
		sc := shippedRecipeFixture(t, "mlx")
		execution := config.ExecutionConfig{Type: "python", Command: "python3", Args: []string{"-m", "different.server"}}
		if runtime == "mlx" {
			sc.Runtime = &config.AppRuntimeConfig{Execution: execution}
		} else {
			sc.Features = map[string]config.Feature{"vision": {Runtime: runtime, Execution: &execution}}
		}
		_, err := ResolveRecipe(sc, "worker", runtime, "")
		require.ErrorContains(t, err, "startup_import must match serving module")
	}
}

func TestSnapshotEnvironmentRefusesUnsafeInputBeforeProbe(t *testing.T) {
	for _, key := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PATH", "PIP_INDEX_URL", "PIP_CONFIG_FILE", "SSL_CERT_FILE"} {
		t.Run(key, func(t *testing.T) {
			_, err := MergeExecutionEnvironment([]string{"PATH=/usr/bin", "PIP_CONFIG_FILE=/dev/null"}, map[string]string{key: "malicious"})
			require.Error(t, err)
		})
	}
	t.Setenv("PIP_INDEX_URL", "https://unapproved.invalid/simple")
	t.Setenv("PYTHONPATH", "/unapproved")
	environment, err := ControlledEnvironment()
	require.NoError(t, err)
	assert.NotContains(t, environment, "PIP_INDEX_URL=https://unapproved.invalid/simple")
	assert.NotContains(t, environment, "PYTHONPATH=/unapproved")
}
